package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/agentprobe"
	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/decision"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
	"agentbox/internal/web"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// This fixture never executes received commands. It supplies synthetic Docker
// and CLI wire output while the actual server performs preparation, UID1000
// writes, role switching, provider transport, SQLite settlement and delivery.
// Run in an owned Linux container; no real Docker socket/accounts are mounted.
func TestCollaborationRuntimeIntegration(t *testing.T) {
	if os.Getenv("AGENTBOX_COLLABORATION_RUNTIME_TEST") != "1" {
		t.Skip("explicit isolated Linux root fixture required")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("runtime integration requires Linux root in an isolated fixture")
	}
	liveDecision := os.Getenv("AGENTBOX_DECISION_LIVE_TEST") == "1"
	var liveSecret struct {
		APIKey        string `json:"api_key"`
		TimeoutMS     int    `json:"timeout_ms"`
		ExpectTimeout bool   `json:"expect_timeout"`
	}
	if liveDecision {
		// Secret material only crosses the parent/fixture stdin pipe. Never put
		// it in argv, Docker environment, files, assertions or evidence output.
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 16<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&liveSecret) != nil || liveSecret.APIKey == "" || len(liveSecret.APIKey) > 8192 || strings.ContainsAny(liveSecret.APIKey, "\r\n\x00") {
			t.Fatal("live decision requires a valid API key supplied through stdin")
		}
		if liveSecret.TimeoutMS == 0 {
			liveSecret.TimeoutMS = 15000
		}
		if liveSecret.TimeoutMS < 100 || liveSecret.TimeoutMS > 30000 {
			t.Fatal("live decision timeout outside allowed range")
		}
	}
	cases := []struct {
		pair     [2]string
		scenario string
	}{}
	for _, pair := range [][2]string{{"claude", "codex"}, {"codex", "claude"}, {"claude", "claude"}, {"codex", "codex"}} {
		cases = append(cases, struct {
			pair     [2]string
			scenario string
		}{pair, ""})
	}
	for _, scenario := range []string{"questions", "verify_failure", "handoff_repair", "pause_resume", "budget", "missing_usage", "cancel_waiting", "revoked", "cancel_running", "decision_auto", "decision_timeout", "reviewed", "direct", "direct_questions", "direct_verify_failure", "direct_repair_questions", "direct_remaining", "direct_no_verification"} {
		cases = append(cases, struct {
			pair     [2]string
			scenario string
		}{[2]string{"codex", "claude"}, scenario})
	}
	cases = append(cases, struct {
		pair     [2]string
		scenario string
	}{[2]string{"claude", "codex"}, "direct"})
	if liveDecision {
		cases = cases[:0]
		cases = append(cases, struct {
			pair     [2]string
			scenario string
		}{[2]string{"codex", "claude"}, "decision_live"})
	}
	for _, testCase := range cases {
		pair, scenario := testCase.pair, testCase.scenario
		direct := strings.HasPrefix(scenario, "direct")
		name := pair[0] + "-" + pair[1]
		if scenario != "" {
			name += "-" + scenario
		}
		t.Run(name, func(t *testing.T) {
			s, source := newTestServer(t)
			s.chat = newChatManager(s)
			image := "sha256:" + strings.Repeat("a", 64)
			head := strings.Repeat("b", 40)
			s.cfg = &config.Config{DataDir: s.cfg.DataDir, AgentImage: image, CollaborationEnabled: true, Resources: config.ResourceLimits{MaxRunning: 1, MaxRunningPerUser: 1}, PermissionMode: "bypassPermissions", Container: config.ContainerLimits{MemoryMB: 256, CPUs: 1, PidsLimit: 64, Network: "none"}, Accounts: []config.Account{{ID: "claude", Type: "claude"}, {ID: "codex", Type: "codex"}}, Pricing: map[string]config.ModelPrice{"claude": {TokenRates: config.TokenRates{Input: 1}}, "codex": {TokenRates: config.TokenRates{Input: 1}}}}
			if scenario == "revoked" {
				s.cfg.AuthToken = "synthetic-fixture-token"
				s.cfg.Listen = "127.0.0.1:8180"
				s.cfg.MaxUploadMB = 10
				s.cfg.TimeZone = "UTC"
				path := filepath.Join(t.TempDir(), "config.json")
				raw, e := json.Marshal(s.cfg)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(path, raw, 0600); e != nil {
					t.Fatal(e)
				}
				loaded, e := config.Load(path)
				if e != nil {
					t.Fatal(e)
				}
				s.cfg = loaded
			}
			u := store.User{Name: "alice", Role: store.RoleUser, CreatedAt: time.Now().UTC()}
			if err := s.store.CreateUser(u); err != nil {
				t.Fatal(err)
			}
			source.Agent = "codex"
			source.AccountID = "codex"
			source.Status = store.StatusStopped
			source.CreatedAt = time.Now()
			if err := s.store.Put(source); err != nil {
				t.Fatal(err)
			}
			if _, err := s.store.Grant(u.Name, 10000, "fixture", "", "admin"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				usage := s.store.ListUsage(store.UsageFilter{User: u.Name})
				spends := s.store.ListLedger(store.LedgerFilter{User: u.Name, Reason: store.ReasonSpend})
				cached, ledger, e := s.store.RecomputeBalance(u.Name)
				chargeable := 0
				for _, row := range usage {
					if row.CostMicroUSD > 0 {
						chargeable++
					}
				}
				if e != nil || cached != ledger || len(spends) != chargeable {
					t.Error("ledger/usage/balance divergence", len(spends), len(usage), cached, ledger, e)
				}
			})
			type box struct {
				container.Config
				HostConfig container.HostConfig
				Running    bool
			}
			type invocation struct {
				Box     string
				Command []string
			}
			var mu sync.Mutex
			boxes := map[string]*box{}
			invocations := map[string]invocation{}
			var sequence, modelCalls, executeCalls, clarifyCalls, verifyCalls atomic.Int32
			var executedModels []string
			var verificationExit atomic.Int32
			holdExecution := make(chan struct{})
			var releaseExecution sync.Once
			defer releaseExecution.Do(func() { close(holdExecution) })
			blob := []byte("original\n")
			hash := sha1.New()
			fmt.Fprintf(hash, "blob %d\x00", len(blob))
			hash.Write(blob)
			oid := hex.EncodeToString(hash.Sum(nil))
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			if err := tw.WriteHeader(&tar.Header{Name: "source.txt", Mode: 0644, Typeflag: tar.TypeReg, Size: int64(len(blob))}); err != nil {
				t.Fatal(err)
			}
			tw.Write(blob)
			tw.Close()
			respond := func(prompt string, b *box) string {
				modelCalls.Add(1)
				marker := "TASK CONTEXT (JSON data):\n"
				parts := strings.SplitN(prompt, marker, 2)
				if len(parts) != 2 {
					t.Error("missing task context")
					return "invalid"
				}
				var c struct {
					Snapshot string `json:"snapshot_id"`
					Messages []struct {
						Type string          `json:"type"`
						Body json.RawMessage `json:"body"`
					} `json:"messages"`
					Plan     int64                         `json:"plan_version"`
					Pending  string                        `json:"pending_question_id"`
					Evidence *collaboration.ReviewEvidence `json:"review_evidence"`
				}
				if err := json.Unmarshal([]byte(parts[1]), &c); err != nil {
					t.Error(err)
				}
				var handoff any
				if strings.HasPrefix(prompt, "HANDOFF REPAIR ONLY:") {
					for _, m := range b.HostConfig.Mounts {
						if m.Target == "/workspace" && !m.ReadOnly {
							t.Error("handoff repair regained write access")
						}
					}
					handoff = map[string]any{"version": 1, "type": "result", "payload": map[string]any{"summary": "Reformatted existing result", "completed": []string{"item"}, "tests": []string{}}}
				} else if strings.Contains(prompt, "this step is plan.") {
					if direct {
						t.Error("direct workflow started a planner")
					}
					handoff = map[string]any{"version": 1, "type": "plan", "payload": map[string]any{"summary": "Synthetic plan", "items": []any{map[string]any{"id": "item", "text": "Implement", "acceptance": []string{"source updated"}}}}}
				} else if strings.Contains(prompt, "this step is clarify.") {
					if clarifyCalls.Add(1) == 1 {
						handoff = map[string]any{"version": 1, "type": "question", "payload": map[string]any{"text": "Should existing accounts be skipped?", "blocking": true, "options": []string{"skip", "replace"}}}
					} else {
						handoff = map[string]any{"version": 1, "type": "answer", "payload": map[string]any{"question_id": c.Pending, "text": "Skip existing accounts"}}
					}
				} else if strings.Contains(prompt, "this step is execute.") || strings.Contains(prompt, "this step is repair.") {
					call := executeCalls.Add(1)
					if ((scenario == "questions" || scenario == "direct_questions") && call == 1) || (scenario == "direct_repair_questions" && call == 2) {
						raw, _ := json.Marshal(map[string]any{"version": 1, "type": "question", "payload": map[string]any{"text": "Which duplicate policy?", "blocking": true}})
						return string(raw)
					}
					for _, m := range b.HostConfig.Mounts {
						if m.Target == "/workspace" {
							if m.ReadOnly {
								t.Error("executor project read-only")
							}
							if err := os.WriteFile(filepath.Join(m.Source, "source.txt"), []byte("implemented\n"), 0644); err != nil {
								t.Error(err)
							}
						}
					}
					if scenario == "cancel_running" && call == 1 {
						<-holdExecution
					}
					if scenario == "handoff_repair" && call == 1 {
						return "Implementation finished, but this is not JSON."
					}
					remaining := []string{}
					if scenario == "direct_remaining" && call == 1 {
						remaining = []string{"finish required assertion"}
					}
					handoff = map[string]any{"version": 1, "type": "result", "payload": map[string]any{"summary": "Implemented", "completed": []string{"item"}, "remaining": remaining, "tests": []string{}}}
				} else if strings.Contains(prompt, "this step is review.") {
					if direct {
						t.Error("direct workflow started a reviewer")
					}
					if scenario == "reviewed" && c.Evidence == nil {
						t.Error("versioned review omitted diff evidence")
					}
					found := false
					for _, m := range c.Messages {
						if m.Type == "platform_verification" {
							var v store.CollaborationVerification
							if json.Unmarshal(m.Body, &v) != nil || v.SnapshotID != c.Snapshot || (v.State != "completed" && v.State != "failed" && v.State != "not_requested") {
								t.Error("review received wrong platform evidence")
							}
							found = true
						}
					}
					if !found {
						t.Error("review ran before platform verification")
					}

					decision := "approve"
					changes := []string{}
					if scenario == "verify_failure" && verifyCalls.Load() == 1 {
						decision = "revise"
						changes = []string{"Repair the failing platform test"}
					}
					handoff = map[string]any{"version": 1, "type": "review", "payload": map[string]any{"decision": decision, "snapshot_id": c.Snapshot, "plan_version": c.Plan, "summary": "Matches plan", "changes": changes}}
				} else {
					t.Error("unexpected collaboration step")
				}
				raw, _ := json.Marshal(handoff)
				return string(raw)
			}
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				switch {
				case strings.HasSuffix(r.URL.Path, "/_ping"):
					w.Header().Set("API-Version", "1.45")
					io.WriteString(w, "OK")
				case strings.Contains(r.URL.Path, "/images/"):
					writeJSON(w, 200, map[string]any{"Id": image, "Os": "linux", "Architecture": "arm64", "Config": map[string]any{"Labels": map[string]string{"agentbox.claude-code": "2.1.280", "agentbox.codex": "0.145.0"}}})
				case strings.HasSuffix(r.URL.Path, "/containers/json"):
					mu.Lock()
					items := []map[string]any{}
					for id, b := range boxes {
						if b.Running {
							items = append(items, map[string]any{"Id": id, "Labels": b.Labels, "State": "running"})
						}
					}
					mu.Unlock()
					writeJSON(w, 200, items)
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					var b box
					if json.NewDecoder(r.Body).Decode(&b) != nil {
						t.Error("bad container create")
					}
					id := fmt.Sprintf("box-%d", sequence.Add(1))
					mu.Lock()
					boxes[id] = &b
					mu.Unlock()
					writeJSON(w, 201, map[string]string{"Id": id})
				case len(p) >= 4 && p[1] == "containers":
					id, action := p[2], p[3]
					mu.Lock()
					b := boxes[id]
					mu.Unlock()
					if b == nil {
						w.WriteHeader(404)
						return
					}
					switch action {
					case "start":
						mu.Lock()
						b.Running = true
						mu.Unlock()
						w.WriteHeader(204)
					case "stop":
						mu.Lock()
						b.Running = false
						mu.Unlock()
						w.WriteHeader(204)
					case "wait":
						writeJSON(w, 200, map[string]int{"StatusCode": 0})
					case "json":
						mu.Lock()
						var mounts []container.MountPoint
						for _, m := range b.HostConfig.Mounts {
							mounts = append(mounts, container.MountPoint{Type: m.Type, Source: m.Source, Destination: m.Target, RW: !m.ReadOnly})
						}
						running := b.Running
						mu.Unlock()
						writeJSON(w, 200, map[string]any{"Id": id, "Image": image, "Config": b.Config, "State": map[string]bool{"Running": running}, "Mounts": mounts})
					case "logs":
						if b.Config.Labels["agentbox.collaboration-probe"] != "1" {
							t.Error("unexpected logs")
						}
						data := collaborationSyntheticProbe(t)
						stdcopy.NewStdWriter(w, stdcopy.Stdout).Write(data)
					case "exec":
						var args struct{ Cmd []string }
						if json.NewDecoder(r.Body).Decode(&args) != nil {
							t.Error("bad exec")
						}
						execID := fmt.Sprintf("exec-%d", sequence.Add(1))
						mu.Lock()
						invocations[execID] = invocation{id, args.Cmd}
						mu.Unlock()
						writeJSON(w, 201, map[string]string{"Id": execID})
					default:
						t.Errorf("unknown container action %s", action)
						w.WriteHeader(404)
					}
				case r.Method == http.MethodDelete && len(p) == 3 && p[1] == "containers":
					mu.Lock()
					delete(boxes, p[2])
					mu.Unlock()
					w.WriteHeader(204)
				case len(p) >= 4 && p[1] == "exec":
					if p[3] == "json" {
						mu.Lock()
						inv := invocations[p[2]]
						target := boxes[inv.Box]
						mu.Unlock()
						code := int32(0)
						if target != nil && target.Labels["agentbox.collaboration-verification"] != "" {
							code = verificationExit.Load()
						}
						writeJSON(w, 200, map[string]any{"Running": false, "ExitCode": code})
						return
					}
					mu.Lock()
					call, ok := invocations[p[2]]
					b := boxes[call.Box]
					mu.Unlock()
					if !ok || b == nil {
						t.Error("missing exec")
						w.WriteHeader(404)
						return
					}
					var start container.ExecStartOptions
					if err := json.NewDecoder(r.Body).Decode(&start); err != nil {
						t.Error(err)
						return
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
					stdout := stdcopy.NewStdWriter(conn, stdcopy.Stdout)
					cmd := strings.Join(call.Command, " ")
					if strings.Contains(cmd, "app-server") {
						scan := bufio.NewScanner(conn)
						scan.Buffer(make([]byte, 65536), 2<<20)
						send := func(v any) { json.NewEncoder(stdout).Encode(v) }
						thread := "thread-" + call.Box
						for scan.Scan() {
							var rpc struct {
								ID     int    `json:"id"`
								Method string `json:"method"`
								Params struct {
									Thread string `json:"threadId"`
									Model  string `json:"model"`
									Input  []struct {
										Text string `json:"text"`
									} `json:"input"`
								} `json:"params"`
							}
							if json.Unmarshal(scan.Bytes(), &rpc) != nil {
								t.Error("bad RPC")
								return
							}
							switch rpc.Method {
							case "initialize":
								send(map[string]any{"id": rpc.ID, "result": map[string]any{}})
							case "thread/start", "thread/resume":
								if rpc.Params.Thread != "" {
									thread = rpc.Params.Thread
								}
								send(map[string]any{"id": rpc.ID, "result": map[string]any{"thread": map[string]string{"id": thread}}})
							case "turn/start":
								if len(rpc.Params.Input) != 1 {
									t.Error("missing prompt")
									return
								}
								mu.Lock()
								executedModels = append(executedModels, rpc.Params.Model)
								mu.Unlock()
								answer := respond(rpc.Params.Input[0].Text, b)
								send(map[string]any{"id": rpc.ID, "result": map[string]any{"turn": map[string]string{"id": "turn"}}})
								send(map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]string{"type": "agentMessage", "text": answer, "phase": "final_answer"}}})
								if scenario != "missing_usage" {
									send(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"tokenUsage": map[string]any{"last": map[string]int{"inputTokens": 10, "outputTokens": 2}}}})
								}
								send(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]string{"status": "completed"}}})
								return
							}
						}
						return
					}
					if strings.Contains(cmd, "claude") && strings.Contains(cmd, "stream-json") {
						prompt, e := io.ReadAll(conn)
						if e != nil {
							t.Error(e)
							return
						}
						answer := respond(string(prompt), b)
						session := "session-" + call.Box
						model := "fixture"
						for i, arg := range call.Command {
							if arg == "--resume" && i+1 < len(call.Command) {
								session = call.Command[i+1]
							}
							if liveDecision && arg == "--model" && i+1 < len(call.Command) {
								model = call.Command[i+1]
							}
						}
						mu.Lock()
						executedModels = append(executedModels, model)
						mu.Unlock()
						json.NewEncoder(stdout).Encode(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"id": fmt.Sprintf("message-%d", modelCalls.Load()), "role": "assistant", "model": model, "stop_reason": "end_turn", "content": []any{map[string]string{"type": "text", "text": answer}}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 2}}})
						json.NewEncoder(stdout).Encode(map[string]any{"type": "result", "subtype": "success", "session_id": session, "result": answer})
						return
					}
					switch {
					case strings.Contains(cmd, "rev-parse"):
						io.WriteString(stdout, head+"\n")
					case strings.Contains(cmd, "ls-tree"):
						fmt.Fprintf(stdout, "100644 blob %s %d\tsource.txt\x00", oid, len(blob))
					case strings.Contains(cmd, "archive --format=tar"):
						stdout.Write(archive.Bytes())
					case strings.Contains(cmd, "/bin/sh -c test -f source.txt"):
						call := verifyCalls.Add(1)
						verificationExit.Store(0)
						if (scenario == "verify_failure" || scenario == "direct_verify_failure" || scenario == "direct_repair_questions") && call == 1 {
							verificationExit.Store(1)
							io.WriteString(stdout, "synthetic assertion failed\n")
						} else {
							io.WriteString(stdout, "verified\n")
						}
					case strings.Contains(cmd, "status --porcelain"), strings.Contains(cmd, "/usr/bin/test"), strings.Contains(cmd, "git "), strings.Contains(cmd, "/usr/bin/git"):
					default:
						t.Errorf("unhandled fixture command %q", cmd)
					}
				default:
					t.Errorf("unhandled Docker request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer func() { releaseExecution.Do(func() { close(holdExecution) }); engine.Close() }()
			t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(engine.URL, "http://"))
			t.Setenv("DOCKER_API_VERSION", "1.45")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CERT_PATH", "")
			var err error
			s.dock, err = dockerx.New(s.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.dock.Close()
			roles := store.CollaborationRoles{Coordinator: store.CollaborationBinding{AgentType: pair[0], AccountID: pair[0], Model: "planner-fixture"}, Executor: store.CollaborationBinding{AgentType: pair[1], AccountID: pair[1], Model: "worker-fixture"}}
			in := store.CollaborationCreateInput{Scope: s.collaborationScope(u), SourceSessionID: source.ID, Repository: ".", Baseline: head, Text: "Implement feature", Roles: roles, AutoExecute: false, MaxSteps: 20, MaxRepairs: 2}
			cfg := store.CollaborationConfig{Roles: roles, AutoExecute: false, MaxSteps: 20, MaxRepairs: 2}
			if direct || scenario == "reviewed" {
				mode := "reviewed"
				if direct {
					mode = "direct"
				}
				in.Workflow = &collaboration.Workflow{Version: 1, Mode: mode}
				cfg.Workflow = in.Workflow
			}
			if pair[0] == "codex" {
				in.VerificationCommand = "test -f source.txt"
				cfg.VerificationCommand = in.VerificationCommand
			}
			if scenario == "direct_no_verification" {
				in.VerificationCommand, cfg.VerificationCommand = "", ""
			}
			if scenario == "budget" {
				in.BudgetMicroUSD = 5
				cfg.BudgetMicroUSD = 5
			}
			var decisionCalls atomic.Int32
			if strings.HasPrefix(scenario, "decision_") && !liveDecision {
				s.cfg.Decision = config.DecisionConfig{Revision: 1, Plugins: config.DecisionPlugins{Jev: config.DecisionJev{Enabled: true, APIKey: "synthetic-never-network", TimeoutMS: 100, Access: &config.AccountAccess{Mode: "all"}}}}
				s.cfg.Models = map[string][]config.ModelOption{"claude": {{ID: "worker-fixture"}, {ID: "economy"}}, "codex": {{ID: "planner-fixture"}}}
				in.Decision = &decision.Intent{Version: 1, Provider: "jev", Roles: map[collaboration.Role]decision.RoleIntent{collaboration.Coordinator: {Mode: "fixed"}, collaboration.Executor: {Mode: "auto"}}}
				s.decisionProvider = func(config.DecisionJev) decision.Provider {
					return fixtureDecisionProvider(func(ctx context.Context, req decision.Request) (decision.Evaluation, error) {
						decisionCalls.Add(1)
						if len(req.Roles) != 1 || len(req.Roles[collaboration.Executor]) != 2 {
							t.Error("unexpected routing candidates", req.Roles)
						}
						if scenario == "decision_timeout" {
							<-ctx.Done()
							return decision.Evaluation{Dispatched: true, Usage: decision.Usage{CostStatus: "unknown"}}, ctx.Err()
						}
						return knownDecision(req), nil
					})
				}
			}
			if liveDecision {
				s.cfg.Decision = config.DecisionConfig{Revision: 1, DefaultProvider: "builtin", FallbackPolicy: "workspace_default", Plugins: config.DecisionPlugins{Jev: config.DecisionJev{Enabled: true, Gateway: "mindshub", Model: config.DecisionJevModel, APIKey: liveSecret.APIKey, TimeoutMS: liveSecret.TimeoutMS, MaxRequestBytes: 32768, MaxConcurrency: 1, Access: &config.AccountAccess{Mode: "all"}}}}
				s.cfg.Models = map[string][]config.ModelOption{
					"claude": {{ID: "worker-fixture", Label: "高能力模型：擅长架构设计、多文件复杂逻辑、并发故障定位；输入价格10美元/百万token"}, {ID: "economy", Label: "经济模型：擅长简单文本替换、格式整理、局部机械修改；输入价格0.1美元/百万token"}},
					"codex":  {{ID: "planner-fixture", Label: "固定规划模型"}},
				}
				s.cfg.Pricing["planner-fixture"] = config.ModelPrice{TokenRates: config.TokenRates{Input: 1}}
				s.cfg.Pricing["worker-fixture"] = config.ModelPrice{TokenRates: config.TokenRates{Input: 10}}
				s.cfg.Pricing["economy"] = config.ModelPrice{TokenRates: config.TokenRates{Input: .1}}
				// Leave decisionProvider nil: the ordinary Go decision service
				// invokes the production NewJev adapter and real Mindshub HTTPS.
			}
			if err = s.store.CreateToken("client-fixture-"+u.Name, u.Name); err != nil {
				t.Fatal(err)
			}
			handler := s.Handler()
			defer func() { s.runtime().cancel(); s.runtime().wg.Wait() }()
			if os.Getenv("AGENTBOX_COLLABORATION_CHAT_BROWSER_TEST") == "1" {
				if err := os.WriteFile(filepath.Join(s.workspaceDir(source), "source.txt"), blob, 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(s.workspaceDir(source), "source.txt"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(s.workspaceDir(source), ".git"), 0755); err != nil {
					t.Fatal(err)
				}
				os.WriteFile(filepath.Join(s.workspaceDir(source), ".git", "config"), []byte("keep metadata"), 0600)
			}
			if os.Getenv("AGENTBOX_COLLABORATION_BROWSER_TEST") == "1" {
				done := make(chan struct{})
				var once sync.Once
				mux := http.NewServeMux()
				mux.HandleFunc("GET /fixture", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprintf(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/css/base.css"><link rel="stylesheet" href="/css/dialogs.css"><link rel="stylesheet" href="/css/select.css"><link rel="stylesheet" href="/css/collaboration.css"></head><body><button id="btn-collaboration" hidden>Open</button><script type="module">import{S}from'/js/state.js';import{initCollaboration}from'/js/features/collaboration/panel.js';import{initI18n}from'/js/i18n.js';S.token='client-fixture-alice';S.user='alice';S.sessions=[{id:%q,name:'Fixture project',agent:'codex'}];S.current=S.sessions[0];initI18n();window.dispose=initCollaboration();</script></body></html>`, source.ID)
				})
				mux.HandleFunc("GET /fixture/chat", func(w http.ResponseWriter, r *http.Request) {
					shell, err := web.FS.ReadFile("static/index.html")
					if err != nil {
						t.Fatal(err)
					}
					html := strings.ReplaceAll(string(shell), "/_v/{{BUILD}}/", "/")
					html = strings.ReplaceAll(html, `<script type="module" src="/js/main.js"></script>`, "")
					script := fmt.Sprintf(`<script type="module">
                    import {S} from '/js/state.js';import {initI18n}from'/js/i18n.js';import{api}from'/js/api.js';import{initChat,loadHistory,sendChat}from'/js/chat.js';
                    const pane=document.getElementById('tab-chat');const dialogs=[...document.querySelectorAll('dialog')];for(const child of document.body.children){if(child!==pane&&!dialogs.includes(child))child.hidden=true;}document.body.append(pane);pane.style.height='100dvh';pane.style.maxWidth='1200px';pane.style.margin='auto';
                    localStorage.setItem('agentbox_token','client-fixture-alice');S.token='client-fixture-alice';const me=await api('/me');S.user=me.user;S.draftScope=me.draft_scope;S.draftProtocol=me.draft_protocol;S.chatScope=me.chat_scope;S.chatProtocol=me.chat_protocol;S.current={id:%q,user:'alice',name:'Current project',agent:'codex',account_id:'codex',default_model:'planner-fixture'};S.sessions=[S.current];S.pick.model='planner-fixture';initI18n();window.dispose=initChat();await loadHistory();window.ready=true;
                    </script>`, source.ID)
					// Only the isolated helper serves this page; no product route.
					html = regexp.MustCompile(`</body>`).ReplaceAllString(html, script+"</body>")
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					io.WriteString(w, html)
				})
				mux.HandleFunc("GET /fixture/evidence", func(w http.ResponseWriter, r *http.Request) {
					quota, _ := s.store.GetQuota(u.Name)
					if liveDecision {
						usage := s.store.ListUsage(store.UsageFilter{User: u.Name})
						var decisionRows, decisionCallsCount int
						var decisionCost, decisionReserved, cliCost int64
						for _, row := range usage {
							if row.Kind == store.UsageKindDecision {
								decisionRows++
								decisionCost += row.CostMicroUSD
							} else {
								cliCost += row.CostMicroUSD
							}
						}
						runs, e := s.store.ListCollaborations(u)
						if e != nil {
							writeJSON(w, 500, map[string]string{"error": "evidence unavailable"})
							return
						}
						for _, run := range runs {
							cost, e := s.store.CollaborationCost(u, run.ID)
							if e != nil {
								writeJSON(w, 500, map[string]string{"error": "evidence unavailable"})
								return
							}
							decisionCallsCount += cost.DecisionCalls
							decisionReserved += cost.DecisionReservedMicroUSD
						}
						mu.Lock()
						models := append([]string(nil), executedModels...)
						mu.Unlock()
						writeJSON(w, 200, map[string]any{"decision_backend": "live_mindshub", "cli_backend": "synthetic", "model_calls": modelCalls.Load(), "executed_models": models, "usage_rows": len(usage), "decision_usage_rows": decisionRows, "decision_calls": decisionCallsCount, "decision_cost_micro_usd": decisionCost, "decision_reserved_micro_usd": decisionReserved, "cli_cost_micro_usd": cliCost, "ledger_spends": len(s.store.ListLedger(store.LedgerFilter{User: u.Name, Reason: store.ReasonSpend})), "balance": quota.BalanceMicroUSD, "usage": usage})
						return
					}
					writeJSON(w, 200, map[string]any{"model_calls": modelCalls.Load(), "usage_rows": len(s.store.ListUsage(store.UsageFilter{User: u.Name})), "ledger_spends": len(s.store.ListLedger(store.LedgerFilter{User: u.Name, Reason: store.ReasonSpend})), "balance": quota.BalanceMicroUSD})
				})
				mux.HandleFunc("POST /fixture/done", func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(done) }); w.WriteHeader(204) })
				mux.Handle("/", handler)
				listener, e := net.Listen("tcp", "0.0.0.0:18888")
				if e != nil {
					t.Fatal(e)
				}
				front := httptest.NewUnstartedServer(mux)
				front.Listener.Close()
				front.Listener = listener
				front.Start()
				defer front.Close()
				select {
				case <-done:
				case <-time.After(150 * time.Second):
					t.Fatal("browser fixture timeout")
				}
				s.runtime().wg.Wait()
				usage := s.store.ListUsage(store.UsageFilter{User: u.Name})
				quota, _ := s.store.GetQuota(u.Name)
				expectedUsage, expectedBalance := 4, int64(9960)
				expectedCalls := int32(4)
				if os.Getenv("AGENTBOX_COLLABORATION_CHAT_BROWSER_TEST") == "1" && !liveDecision {
					expectedUsage, expectedBalance, expectedCalls = 5, 9950, 5
				}
				if liveDecision {
					expectedUsage, expectedBalance = 5, 9969
					if liveSecret.ExpectTimeout {
						expectedUsage, expectedBalance = 4, 9870
					}
				}
				if modelCalls.Load() != expectedCalls || len(usage) != expectedUsage || quota.BalanceMicroUSD != expectedBalance {
					t.Fatal("browser settlement mismatch", modelCalls.Load(), len(usage), quota)
				}
				return
			}
			requestID := store.NewCollaborationID()
			raw, _ := json.Marshal(in)
			createURL := "/api/collaboration/runs/" + requestID
			response := clientRequest(handler, u.Name, "PUT", createURL, string(raw))
			if response.Code != 202 {
				t.Fatal(response.Code, response.Body.String())
			}
			var accepted struct {
				Run store.CollaborationRun `json:"run"`
			}
			if err = json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
				t.Fatal(err)
			}
			r := accepted.Run
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			awaitState := func(want string) {
				t.Helper()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					response := clientRequest(handler, u.Name, "GET", createURL, "")
					if response.Code != 200 {
						t.Fatal(response.Code, response.Body.String())
					}
					accepted.Run = store.CollaborationRun{}
					if err = json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
						t.Fatal(err)
					}
					r = accepted.Run
					if r.State == want {
						return
					}
					if r.State == store.CollabFailed || r.State == store.CollabUncertain {
						t.Fatal("runtime failed", r)
					}
					select {
					case <-ctx.Done():
						t.Fatal("task did not reach", want, r)
					case <-ticker.C:
					}
				}
			}
			if scenario == "missing_usage" {
				awaitState(store.CollabUncertain)
				s.runtime().wg.Wait()
				work, e := s.store.CollaborationWork(u, r.ID)
				if e != nil {
					t.Fatal(e)
				}
				history := clientRequest(handler, u.Name, "GET", "/api/collaboration/runs/"+r.ID+"/steps/"+work.Step.ID+"/history", "")
				if history.Code != 200 {
					t.Fatal(history.Code, history.Body.String())
				}
				usage := s.store.ListUsage(store.UsageFilter{User: u.Name})
				quota, _ := s.store.GetQuota(u.Name)
				if modelCalls.Load() != 1 || len(usage) != 0 || quota.BalanceMicroUSD != 10000 {
					t.Fatal("unobserved usage advanced", modelCalls.Load(), usage, quota)
				}
				return
			}
			action := func(kind string, body any) {
				t.Helper()
				input := map[string]any{"scope": in.Scope, "expected_revision": r.Revision, "action": kind}
				if body != nil {
					input["payload"] = body
				}
				payload, _ := json.Marshal(input)
				response := clientRequest(handler, u.Name, "PUT", "/api/collaboration/runs/"+r.ID+"/actions/"+store.NewCollaborationID(), string(payload))
				if response.Code != 200 {
					t.Fatal(kind, response.Code, response.Body.String())
				}
				r, _ = s.store.Collaboration(u, r.ID)
			}
			expectedCalls := int32(3)
			if direct {
				expectedCalls = 1
				if scenario == "direct_questions" || scenario == "direct_repair_questions" {
					awaitState(store.CollabWaitingUser)
					beforeAnswer := int32(1)
					if scenario == "direct_repair_questions" {
						beforeAnswer = 2
					}
					if modelCalls.Load() != beforeAnswer {
						t.Fatal("question invoked an unused coordinator", modelCalls.Load())
					}
					action("answer", map[string]any{"question_id": r.PendingQuestion, "text": "Skip existing accounts"})
					expectedCalls = beforeAnswer + 1
				}
				if scenario == "direct_verify_failure" || scenario == "direct_remaining" {
					expectedCalls = 2
				}
			} else {
				awaitState(store.CollabAwaitingPlan)
				if scenario == "cancel_waiting" {
					action("cancel", nil)
					awaitState(store.CollabCancelled)
					s.runtime().wg.Wait()
					if modelCalls.Load() != 1 {
						t.Fatal("cancel dispatched work")
					}
					return
				}
				if scenario == "revoked" {
					if _, e := s.cfg.UpdateAccount("claude", config.AccountPatch{Access: &config.AccountAccess{Mode: "admin"}}); e != nil {
						t.Fatal(e)
					}
				}
				if scenario == "pause_resume" {
					action("pause", nil)
					awaitState(store.CollabPaused)
					action("resume", nil)
					awaitState(store.CollabAwaitingPlan)
				}
				if modelCalls.Load() != 1 {
					t.Fatal("executor bypassed user plan confirmation", modelCalls.Load())
				}
				payload, _ := json.Marshal(map[string]any{"scope": in.Scope, "expected_revision": r.Revision, "action": "confirm_plan", "payload": map[string]any{"plan_version": r.PlanVersion}})
				actionURL := "/api/collaboration/runs/" + r.ID + "/actions/" + store.NewCollaborationID()
				response = clientRequest(handler, u.Name, "PUT", actionURL, string(payload))
				if response.Code != 200 {
					t.Fatal(response.Code, response.Body.String())
				}
				response = clientRequest(handler, u.Name, "PUT", actionURL, string(payload))
				if response.Code != 200 || !strings.Contains(response.Body.String(), `"replayed":true`) {
					t.Fatal("action replay", response.Code, response.Body.String())
				}
				switch scenario {
				case "cancel_running":
					for modelCalls.Load() < 2 {
						select {
						case <-ctx.Done():
							t.Fatal("executor did not start")
						case <-time.After(10 * time.Millisecond):
						}
					}
					r, _ = s.store.Collaboration(u, r.ID)
					action("cancel", nil)
					awaitState(store.CollabUncertain)
					releaseExecution.Do(func() { close(holdExecution) })
					s.runtime().wg.Wait()
					snapshotBefore, e := s.store.CollaborationLatestArtifact(u, r.ID)
					if e != nil {
						t.Fatal(e)
					}
					inspectInput, _ := json.Marshal(map[string]any{"scope": in.Scope, "expected_revision": r.Revision})
					inspection := clientRequest(handler, u.Name, "POST", "/api/collaboration/runs/"+r.ID+"/inspection", string(inspectInput))
					if inspection.Code != 200 {
						t.Fatal(inspection.Code, inspection.Body.String())
					}
					var report struct {
						Artifact store.CollaborationArtifact `json:"artifact"`
					}
					if json.Unmarshal(inspection.Body.Bytes(), &report) != nil || report.Artifact.Kind != "inspection" {
						t.Fatal(inspection.Body.String())
					}
					diff := clientRequest(handler, u.Name, "GET", "/api/collaboration/runs/"+r.ID+"/diff?snapshot="+report.Artifact.ID+"&path=source.txt", "")
					if diff.Code != 200 || !strings.Contains(diff.Body.String(), "+implemented") {
						t.Fatal(diff.Code, diff.Body.String())
					}
					snapshotAfter, e := s.store.CollaborationLatestArtifact(u, r.ID)
					if e != nil || snapshotBefore.ID != snapshotAfter.ID {
						t.Fatal("inspection promoted partial work", e)
					}
					work, e := s.store.CollaborationWork(u, r.ID)
					if e != nil {
						t.Fatal(e)
					}
					action("review", map[string]any{"step_id": work.Step.ID, "step_revision": work.Step.Revision, "acknowledged": true})
					action("cancel", nil)
					awaitState(store.CollabCancelled)
					s.runtime().wg.Wait()
					usage := s.store.ListUsage(store.UsageFilter{User: u.Name})
					quota, _ := s.store.GetQuota(u.Name)
					if modelCalls.Load() != 2 || len(usage) != 1 || quota.BalanceMicroUSD != 9990 {
						t.Fatal("cancel replay/settlement", modelCalls.Load(), len(usage), quota)
					}
					return
				case "questions":
					awaitState(store.CollabWaitingUser)
					action("answer", map[string]any{"question_id": r.PendingQuestion, "text": "Skip existing accounts"})
					expectedCalls = 6
				case "handoff_repair":
					awaitState(store.CollabPaused)
					if r.WaitReason != "handoff_invalid" {
						t.Fatal(r)
					}
					action("repair_handoff", nil)
					expectedCalls = 4
				case "verify_failure":
					expectedCalls = 5
				case "budget", "revoked":
					awaitState(store.CollabPaused)
					if (scenario == "budget" && r.WaitReason != "budget_limit") || modelCalls.Load() != 1 {
						t.Fatal(r, modelCalls.Load())
					}
					s.runtime().wg.Wait()
					usage := s.store.ListUsage(store.UsageFilter{User: u.Name})
					quota, _ := s.store.GetQuota(u.Name)
					if len(usage) != 1 || quota.BalanceMicroUSD != 9990 {
						t.Fatal(usage, quota)
					}
					return
				}
			}
			awaitState(store.CollabCompleted)
			if err != nil || r.State != store.CollabCompleted || (!direct && cfg.VerificationCommand == "" && r.WaitReason != "model_review_only") || (!direct && cfg.VerificationCommand != "" && r.WaitReason != "") {
				t.Fatal(r, err)
			}
			if direct {
				participants, e := s.store.CollaborationParticipants(u, r.ID)
				if e != nil || len(participants) != 1 || participants[0].Role != "executor" {
					t.Fatal("direct workflow created another participant", participants, e)
				}
			}
			if modelCalls.Load() != expectedCalls {
				t.Fatal("unexpected model calls", modelCalls.Load())
			}
			usage := s.store.ListUsage(store.UsageFilter{User: u.Name})
			quota, _ := s.store.GetQuota(u.Name)
			expectedUsage, expectedBalance := int(expectedCalls), 10000-int64(expectedCalls)*10
			if scenario == "decision_auto" {
				expectedUsage++
				expectedBalance -= 42
			}
			if len(usage) != expectedUsage || quota.BalanceMicroUSD != expectedBalance {
				t.Fatal("settlement mismatch", len(usage), quota)
			}
			if strings.HasPrefix(scenario, "decision_") {
				d, e := s.store.CollaborationDecision(u, r.ID)
				if e != nil || d.Result == nil || decisionCalls.Load() != 1 || d.Result.Roles.Coordinator != roles.Coordinator || r.Request.Roles != roles {
					t.Fatal("decision integration mismatch", d, e, decisionCalls.Load())
				}
				wantModel, wantReserved := "economy", int64(0)
				if scenario == "decision_timeout" {
					wantModel, wantReserved = roles.Executor.Model, 2753
				}
				cost, e := s.store.CollaborationCost(u, r.ID)
				if e != nil || d.Result.Roles.Executor.Model != wantModel || cost.DecisionReservedMicroUSD != wantReserved {
					t.Fatal(d.Result, cost, e)
				}
			}
			response = clientRequest(handler, u.Name, "PUT", createURL, string(raw))
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"replayed":true`) || modelCalls.Load() != expectedCalls {
				t.Fatal("creation replay", response.Code, response.Body.String())
			}
			// Join the scheduler before asserting its final invocation count.
			s.runtime().wg.Wait()
			if modelCalls.Load() != expectedCalls {
				t.Fatal("HTTP replay scheduled duplicate work", modelCalls.Load())
			}
			artifact, err := s.store.CollaborationLatestArtifact(u, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(s.collaborationDir(u, r.ID), artifact.StorageKey, "tree", "source.txt"))
			if err != nil || string(data) != "implemented\n" {
				t.Fatal(string(data), err)
			}

			if scenario == "" {
				// Real filesystem publication with the synthetic container/Git
				// boundary; no model replay and no extra usage on application.
				project := s.workspaceDir(source)
				if err := os.WriteFile(filepath.Join(project, "source.txt"), blob, 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(project, "source.txt"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(project, ".git"), 0755); err != nil {
					t.Fatal(err)
				}
				os.WriteFile(filepath.Join(project, ".git", "config"), []byte("keep metadata"), 0600)
				os.WriteFile(filepath.Join(project, "ignored-cache"), []byte("keep ignored"), 0644)
				action := store.NewCollaborationID()
				body, _ := json.Marshal(map[string]any{"scope": in.Scope, "expected_revision": r.Revision, "snapshot_id": artifact.ID})
				path := "/api/collaboration/runs/" + r.ID + "/application/" + action
				response := clientRequest(handler, u.Name, "PUT", path, string(body))
				if response.Code != 200 {
					t.Fatal("apply", response.Code, response.Body.String())
				}
				for file, want := range map[string]string{"source.txt": "implemented\n", ".git/config": "keep metadata", "ignored-cache": "keep ignored"} {
					got, err := os.ReadFile(filepath.Join(project, file))
					if err != nil || string(got) != want {
						t.Fatal("application lost file", file, string(got), err)
					}
				}
				response = clientRequest(handler, u.Name, "PUT", path, string(body))
				if response.Code != 200 || !strings.Contains(response.Body.String(), `"replayed":true`) {
					t.Fatal("apply replay", response.Code, response.Body.String())
				}
				if modelCalls.Load() != expectedCalls {
					t.Fatal("application invoked model")
				}
				prior, err := os.ReadFile(filepath.Join(filepath.Dir(project), "collaboration-before-"+action, "source.txt"))
				if err != nil || string(prior) != string(blob) {
					t.Fatal("original backup missing", err)
				}
			}
			participants, err := s.store.CollaborationParticipants(u, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range participants {
				if p.ProcessEpoch != p.StoppedEpoch {
					t.Fatal("live participant after completion", p)
				}
			}
		})
	}
}

func collaborationSyntheticProbe(t *testing.T) []byte {
	t.Helper()
	var input struct {
		Handoffs map[string]json.RawMessage `json:"handoffs"`
	}
	if err := json.Unmarshal(agentprobe.CollaborationInput(), &input); err != nil {
		t.Fatal(err)
	}
	var records []any
	for _, kind := range []string{"claude", "codex"} {
		for _, phase := range []string{"plan", "result"} {
			resume := ""
			rpcID := 3
			if phase == "result" {
				resume = "probe-thread"
				rpcID = 2
			}
			record := map[string]any{"name": kind + "_" + phase, "agent": kind, "requests": 1, "exit_code": 0, "resume": resume}
			if kind == "claude" {
				record["lines"] = []any{map[string]any{"type": "result", "subtype": "success", "session_id": "probe-thread", "result": string(input.Handoffs[phase])}}
			} else {
				record["rpc"] = []any{map[string]any{"id": 1, "result": map[string]any{}}, map[string]any{"id": rpcID, "result": map[string]any{"thread": map[string]string{"id": "probe-thread"}}}, map[string]any{"id": 4, "result": map[string]any{"turn": map[string]string{"id": "probe-turn"}}}, map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]string{"type": "agentMessage", "text": string(input.Handoffs[phase])}}}, map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]string{"status": "completed"}}}}
			}
			records = append(records, record)
		}
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "records": records})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
