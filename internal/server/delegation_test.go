package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/decision"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gorilla/websocket"
)

// delegationFixture runs real HTTP receipts, the per-turn bridge and the chat
// service against a fake Docker Engine. The fake "main agent" calls the
// delegate tool over the bridge exactly as the MCP helper would; the fake
// worker edits the workspace and reports usage. No CLI or provider runs.
type delegationFixture struct {
	codex      bool
	created    atomic.Value
	s          *Server
	sess       store.Session
	u          store.User
	h          http.Handler
	thread     string
	task       map[string]any
	mainCmd    atomic.Value
	mainInput  atomic.Value
	workerCmds atomic.Int32
	workerArgs atomic.Value
	reply      atomic.Value
}

const workerReport = "## Done\\nadded add()\\n## Commands and results\\ngo test ./... passed\\n## Open issues\\nnone"

func newDelegationFixture(t *testing.T, task map[string]any) *delegationFixture {
	return newDelegationFixtureWith(t, task, false)
}

func newDelegationFixtureWith(t *testing.T, task map[string]any, codex bool) *delegationFixture {
	t.Helper()
	s, sess := newTestServer(t)
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{
		"data_dir": s.cfg.DataDir, "auth_token": "synthetic-test-token",
		"proxy_bridge": map[string]string{"bind": "127.0.0.1:1"},
		"accounts": []map[string]any{{
			"id": "main", "type": "claude", "default_model": "claude-opus-5-5",
			"models": []map[string]any{
				{"id": "claude-opus-5-5", "label": "Opus"},
				{"id": "claude-sonnet-5-5", "label": "Sonnet", "delegation": map[string]any{"enabled": true, "tier": "economy", "can": []string{"implement_spec", "write_tests"}, "avoid": []string{"billing"}, "notes": "按接口实现稳定"}},
			},
		}, {
			"id": "cx", "type": "codex", "default_model": "gpt-5.5-mini",
			"models": []map[string]any{{"id": "gpt-5.5-mini", "label": "Mini", "delegation": map[string]any{"enabled": true, "can": []string{"implement_spec"}}}},
		}},
		"pricing": map[string]any{"claude-sonnet-5-5": map[string]float64{"input": 3, "output": 15}, "gpt-5.5-mini": map[string]float64{"input": 1, "output": 4}},
	})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	if s.cfg, err = config.Load(path); err != nil {
		t.Fatal(err)
	}
	if err = s.store.CreateUser(store.User{Name: "alice", Role: store.RoleUser}); err != nil {
		t.Fatal(err)
	}
	if err = s.store.CreateToken("alice", "alice"); err != nil {
		t.Fatal(err)
	}
	u, _ := s.store.GetUser("alice")
	sess.Agent, sess.AccountID, sess.DefaultModel, sess.Status, sess.ContainerID = config.AgentClaude, "main", "claude-opus-5-5", store.StatusRunning, "fixture"
	if err = s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	s.chat = newChatManager(s)
	s.upgrader = websocket.Upgrader{}
	if err = os.MkdirAll(s.homeDir(sess), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = s.migrateThreads(sess); err != nil {
		t.Fatal(err)
	}
	tid, err := s.ensureActiveThread(sess)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []logEntry{{Kind: "user", Text: "synthetic previous turn"}, {Kind: "title", Text: "Synthetic title"}} {
		if err = s.appendThreadEntry(sess, tid, e); err != nil {
			t.Fatal(err)
		}
	}
	f := &delegationFixture{s: s, sess: sess, u: u, thread: tid, task: task, codex: codex}
	var commands sync.Map
	var ids atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.45")
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/containers/fixture/json"):
			_, _ = w.Write([]byte(`{"Id":"fixture","State":{"Running":true},"Mounts":[{"Destination":"/shared"}]}`))
		case strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			_, _ = w.Write([]byte(`{"Id":"sha256:synthetic","Os":"linux","Config":{"Labels":{}}}`))
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			var body struct{ HostConfig struct{ Binds []string } }
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.created.Store(body.HostConfig.Binds)
			writeJSON(w, 201, map[string]string{"Id": "worker-container"})
		case strings.HasSuffix(r.URL.Path, "/containers/worker-container/start"):
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/containers/worker-container/json"):
			_, _ = w.Write([]byte(`{"Id":"worker-container","State":{"Running":true},"Mounts":[{"Destination":"/shared"}]}`))
		case strings.HasSuffix(r.URL.Path, "/containers/fixture/exec") || strings.HasSuffix(r.URL.Path, "/containers/worker-container/exec"):
			var body struct{ Cmd []string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			id := fmt.Sprintf("exec-%d", ids.Add(1))
			commands.Store(id, body.Cmd)
			writeJSON(w, 201, map[string]string{"Id": id})
		case strings.Contains(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/start"):
			parts := strings.Split(r.URL.Path, "/")
			value, _ := commands.Load(parts[len(parts)-2])
			cmd, _ := value.([]string)
			var opts struct{ Detach bool }
			_ = json.NewDecoder(r.Body).Decode(&opts)
			if opts.Detach {
				w.WriteHeader(200)
				return
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			if strings.Contains(strings.Join(cmd, " "), "app-server") {
				return // the handshake fails, so the worker falls back to codex exec
			}
			input, err := io.ReadAll(conn)
			if err != nil {
				return
			}
			out := stdcopy.NewStdWriter(conn, stdcopy.Stdout)
			if flag := argAfter(cmd, "--mcp-config"); flag != "" {
				f.mainCmd.Store(cmd)
				f.mainInput.Store(string(input))
				f.reply.Store(f.callDelegate(t, flag))
				_, _ = io.WriteString(out, `{"type":"assistant","session_id":"main-session","message":{"id":"msg-main","model":"claude-opus-5-5","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}}`+"\n"+
					`{"type":"result","subtype":"success","session_id":"main-session","result":"done"}`+"\n")
				return
			}
			f.workerCmds.Add(1)
			f.workerArgs.Store(cmd)
			if err := os.WriteFile(filepath.Join(s.workspaceDir(sess), "calc.go"), []byte("package calc\n"), 0o644); err != nil {
				t.Error(err)
			}
			if strings.Contains(strings.Join(cmd, " "), "codex exec") {
				_, _ = io.WriteString(out, `{"type":"thread.started","thread_id":"worker-thread"}`+"\n"+
					`{"type":"item.completed","item":{"type":"agent_message","text":"## Done\nadded add() with codex"}}`+"\n"+
					`{"type":"turn.completed","usage":{"input_tokens":1000,"output_tokens":100}}`+"\n")
				return
			}
			_, _ = io.WriteString(out, `{"type":"system","subtype":"init","session_id":"worker-session"}`+"\n"+
				`{"type":"assistant","session_id":"worker-session","message":{"id":"msg-worker","model":"claude-sonnet-5-5","stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":200}}}`+"\n"+
				`{"type":"result","subtype":"success","session_id":"worker-session","result":"`+workerReport+`"}`+"\n")
		case strings.Contains(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/json"):
			_, _ = w.Write([]byte(`{"Running":false,"ExitCode":0}`))
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(engine.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.45")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	if s.dock, err = dockerx.New(s.cfg); err != nil {
		t.Fatal(err)
	}
	f.h = s.Handler()
	t.Cleanup(func() { closeTestServer(t, s); engine.Close() })
	return f
}

func argAfter(cmd []string, flag string) string {
	for i, a := range cmd {
		if a == flag && i+1 < len(cmd) {
			return cmd[i+1]
		}
	}
	return ""
}

// callDelegate is the fake main agent's tool call, the way delegate_mcp.py
// relays it: POST to the bridge with the per-turn ticket.
func (f *delegationFixture) callDelegate(t *testing.T, mcp string) map[string]any {
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			Timeout int64             `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(mcp), &cfg); err != nil {
		t.Error(err)
		return nil
	}
	server := cfg.MCPServers["agentbox"]
	if server.Command != "python3" || server.Args[len(server.Args)-1] != delegationHelperPath || server.Timeout < int64(time.Hour/time.Millisecond) {
		t.Error("unexpected MCP server definition", server)
	}
	var tools []map[string]any
	if json.Unmarshal([]byte(server.Env["AGENTBOX_DELEGATE_TOOLS"]), &tools) != nil || len(tools) != 2 || tools[0]["name"] != "delegate" {
		t.Error("tool list", server.Env["AGENTBOX_DELEGATE_TOOLS"])
	}
	body := map[string]any{"call_id": "1"}
	for k, v := range f.task {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, server.Env["AGENTBOX_DELEGATE_URL"]+"/delegate", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+server.Env["AGENTBOX_DELEGATE_TOKEN"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return nil
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Error(err)
	}
	// A stolen ticket from another turn or no ticket is refused.
	bad, _ := http.NewRequest(http.MethodPost, server.Env["AGENTBOX_DELEGATE_URL"]+"/delegate", strings.NewReader(string(raw)))
	if r2, err := http.DefaultClient.Do(bad); err == nil {
		if r2.StatusCode != http.StatusUnauthorized {
			t.Error("bridge accepted a call without the ticket", r2.StatusCode)
		}
		r2.Body.Close()
	}
	return out
}

func (f *delegationFixture) send(t *testing.T, jev bool) store.ChatRequest {
	t.Helper()
	roster := []store.DelegationWorker{{AccountID: "main", Model: "claude-sonnet-5-5"}}
	if f.codex {
		roster = []store.DelegationWorker{{AccountID: "cx", Model: "gpt-5.5-mini"}}
	}
	input := store.ChatRequestInput{Scope: f.s.chatRequestScope(f.u), ThreadID: f.thread, Text: "implement add", Model: f.sess.DefaultModel,
		Delegation: &store.DelegationOptions{Version: 1, Workers: roster, Jev: jev}}
	raw, _ := json.Marshal(input)
	id := newOperationID()
	r := httptest.NewRequest(http.MethodPut, "/api/sessions/"+f.sess.ID+"/chat/requests/"+id, strings.NewReader(string(raw)))
	r.Header.Set("Authorization", "Bearer alice")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 200 && w.Code != 202 {
		t.Fatalf("send %d %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := f.s.store.ChatRequest(f.u, f.sess.ID, id)
		if err == nil && f.s.chat.room(f.sess.ID).state() == "idle" && c.State != store.ChatAccepted && c.State != store.ChatStarting && c.State != store.ChatRunning {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("turn did not settle")
	return store.ChatRequest{}
}

func (f *delegationFixture) delegations(t *testing.T) []store.Delegation {
	t.Helper()
	out, err := f.s.store.SessionDelegations(f.sess.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *delegationFixture) historyKinds(t *testing.T) map[string]int {
	t.Helper()
	file, err := os.Open(f.s.threadPath(f.sess, f.thread))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	kinds := map[string]int{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 8<<20)
	for scanner.Scan() {
		var e logEntry
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			kinds[e.Kind]++
			if e.Kind == "user" && strings.Contains(e.Text, "agentbox-delegation") {
				t.Fatal("the delegation reminder leaked into history", e.Text)
			}
		}
	}
	return kinds
}

func TestDelegationRoutesRunsAndSettlesWorker(t *testing.T) {
	f := newDelegationFixture(t, map[string]any{"task": "Implement add(a, b int) int in calc.go", "task_type": "implement_spec", "risks": []string{}, "access": "write"})
	receipt := f.send(t, false)
	if receipt.State != store.ChatCompleted {
		t.Fatal("main turn", receipt.State, receipt.ErrorCode)
	}
	reply, _ := f.reply.Load().(map[string]any)
	changes, _ := reply["changed_files"].(map[string]any)
	if reply["status"] != "completed" || !strings.Contains(fmt.Sprint(reply["report"]), "added add()") || fmt.Sprint(changes["added"]) != "[calc.go]" {
		t.Fatalf("tool reply %+v", reply)
	}
	main, _ := f.mainCmd.Load().([]string)
	if argAfter(main, "--disallowedTools") != "Agent,Task" || argAfter(main, "--allowedTools") != "mcp__agentbox__delegate,mcp__agentbox__delegation_feedback" || !strings.Contains(argAfter(main, "--append-system-prompt"), "mcp__agentbox__delegate") || !strings.Contains(strings.Join(main, " "), "MCP_TOOL_TIMEOUT=") {
		t.Fatal("main command lacks delegation extras", main)
	}
	if input, _ := f.mainInput.Load().(string); !strings.HasPrefix(input, "<agentbox-delegation>") || !strings.Contains(input, "select:mcp__agentbox__delegate") {
		t.Fatal("the main agent did not receive the per-turn delegation reminder", input)
	}
	worker, _ := f.workerArgs.Load().([]string)
	if f.workerCmds.Load() != 1 || argAfter(worker, "--model") != "claude-sonnet-5-5" || argAfter(worker, "--mcp-config") != "" || argAfter(worker, "--resume") != "" || !strings.Contains(strings.Join(worker, " "), "/tmp/.agentbox-delegate-") {
		t.Fatal("worker command", worker)
	}
	ds := f.delegations(t)
	if len(ds) != 1 || ds[0].State != store.DelegationCompleted || ds[0].Model != "claude-sonnet-5-5" || ds[0].ProviderSession != "worker-session" || ds[0].WorkerSessionID != "" {
		t.Fatalf("%+v", ds)
	}
	var usage []store.UsageEvent
	for _, row := range f.s.store.ListUsage(store.UsageFilter{Delegation: "only"}) {
		usage = append(usage, row)
	}
	if len(usage) != 1 || usage[0].SessionID != f.sess.ID || usage[0].ThreadID != f.thread || usage[0].DelegationID != ds[0].ID || usage[0].CostMicroUSD != 6000 {
		t.Fatalf("worker usage %+v", usage)
	}
	if main := f.s.store.ListUsage(store.UsageFilter{Delegation: "exclude"}); len(main) != 1 || main[0].Model != "claude-opus-5-5" {
		t.Fatalf("main usage %+v", main)
	}
	if kinds := f.historyKinds(t); kinds["delegation"] < 2 {
		t.Fatal("history lacks the delegation card", kinds)
	}
	if _, err := os.Stat(filepath.Join(f.s.homeDir(f.sess), ".agentbox", "delegate-mcp.py")); err != nil {
		t.Fatal("MCP helper not installed", err)
	}
	logFile := filepath.Join(f.s.sessionDir(f.sess), "delegations", ds[0].ID+".jsonl")
	if raw, err := os.ReadFile(logFile); err != nil || !strings.Contains(string(raw), "worker-session") {
		t.Fatal("worker transcript", err)
	}
	// The worker transcript is readable by its owner only.
	r := httptest.NewRequest(http.MethodGet, "/api/sessions/"+f.sess.ID+"/delegations/"+ds[0].ID+"/log", nil)
	r.Header.Set("Authorization", "Bearer alice")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "worker-session") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestDelegationDeclinesRiskTheWorkerAvoids(t *testing.T) {
	f := newDelegationFixture(t, map[string]any{"task": "Change billing rounding", "task_type": "implement_spec", "risks": []string{"billing"}, "access": "write"})
	if receipt := f.send(t, false); receipt.State != store.ChatCompleted {
		t.Fatal(receipt.State)
	}
	reply, _ := f.reply.Load().(map[string]any)
	if reply["status"] != "declined" || reply["reason"] != "no_eligible_worker" || f.workerCmds.Load() != 0 {
		t.Fatalf("%+v workers=%d", reply, f.workerCmds.Load())
	}
	ds := f.delegations(t)
	if len(ds) != 1 || ds[0].State != store.DelegationDeclined || !strings.Contains(string(ds[0].Route), "risk:billing") {
		t.Fatalf("%+v", ds)
	}
}

type fakeDecision struct {
	choice string
	calls  atomic.Int32
}

func (p *fakeDecision) ID() string { return "jev" }
func (p *fakeDecision) Capabilities() decision.Capabilities {
	return decision.Capabilities{Choice: true}
}
func (p *fakeDecision) Evaluate(_ context.Context, req decision.Request) (decision.Evaluation, error) {
	p.calls.Add(1)
	options := req.Questions["worker"]
	if len(options) != 2 || options[1].Ref != decision.SelfRef || strings.Contains(options[0].Description, "main") {
		return decision.Evaluation{}, decision.ErrInvalid
	}
	return decision.Evaluation{Provider: "jev", Model: config.DecisionJevModel, Status: "suggest", Dispatched: true,
		Suggestions: map[string]decision.Suggestion{"worker": {Status: "suggest", CandidateRef: p.choice, Reason: "provider_choice", Confidence: 0.9}},
		Usage:       decision.Usage{InputTokens: 500, Known: true, CostStatus: "known", CostMicroUSD: 21}}, nil
}

func TestDelegationJevCanKeepSubtaskWithMainAgent(t *testing.T) {
	f := newDelegationFixture(t, map[string]any{"task": "Design the plugin API", "task_type": "implement_spec", "risks": []string{}, "access": "write"})
	enabled := true
	key := config.DecisionSecretPatch{Action: "set", Value: "synthetic-key"}
	rev := f.s.cfg.GetDecision().Revision
	if err := f.s.cfg.ApplySettings(config.SettingsPatch{Decision: &config.DecisionPatch{ExpectedRevision: &rev, Plugins: &config.DecisionPluginsPatch{Jev: &config.DecisionJevPatch{Enabled: &enabled, APIKey: &key, Access: &config.AccountAccess{Mode: "all"}}}}}); err != nil {
		t.Fatal(err)
	}
	provider := &fakeDecision{choice: decision.SelfRef}
	f.s.decisionProvider = func(config.DecisionJev) decision.Provider { return provider }
	if receipt := f.send(t, true); receipt.State != store.ChatCompleted {
		t.Fatal(receipt.State)
	}
	reply, _ := f.reply.Load().(map[string]any)
	if reply["status"] != "declined" || provider.calls.Load() != 1 || f.workerCmds.Load() != 0 {
		t.Fatalf("%+v calls=%d", reply, provider.calls.Load())
	}
	ds := f.delegations(t)
	if len(ds) != 1 || !strings.Contains(string(ds[0].Route), `"source":"jev"`) {
		t.Fatalf("%+v", ds)
	}
	rows := f.s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindDecision})
	if len(rows) != 1 || rows[0].DelegationID != ds[0].ID || rows[0].CostMicroUSD != 21 {
		t.Fatalf("decision usage %+v", rows)
	}
}

// A worker on another account and CLI gets its own hidden workspace whose
// /workspace is the owner's directory; its spend is attributed to the owner.
func TestDelegationRunsOtherCLIInHiddenWorkerWorkspace(t *testing.T) {
	if uid := os.Geteuid(); uid != 0 && uid != 1000 {
		t.Skip("requires chown to container UID; exercised in Linux container suite")
	}
	f := newDelegationFixtureWith(t, map[string]any{"task": "Implement add(a, b int) int in calc.go", "task_type": "implement_spec", "risks": []string{}, "access": "write"}, true)
	if receipt := f.send(t, false); receipt.State != store.ChatCompleted {
		t.Fatal(receipt.State, receipt.ErrorCode)
	}
	reply, _ := f.reply.Load().(map[string]any)
	if reply["status"] != "completed" || !strings.Contains(fmt.Sprint(reply["report"]), "with codex") {
		t.Fatalf("%+v", reply)
	}
	binds, _ := f.created.Load().([]string)
	ds := f.delegations(t)
	if len(ds) != 1 || ds[0].WorkerSessionID == "" || ds[0].Agent != config.AgentCodex || ds[0].ProviderSession != "worker-thread" {
		t.Fatalf("%+v", ds)
	}
	worker, ok := f.s.store.Get(ds[0].WorkerSessionID)
	if !ok || worker.ContainerID != "worker-container" {
		t.Fatal("worker workspace", worker, ok)
	}
	want := map[string]bool{f.s.workspaceDir(f.sess) + ":/workspace": false, f.s.homeDir(worker) + ":/home/agent": false}
	for _, b := range binds {
		if _, ok := want[b]; ok {
			want[b] = true
		}
	}
	for b, seen := range want {
		if !seen {
			t.Fatal("missing worker mount", b, binds)
		}
	}
	public, _ := f.s.store.PublicSessions("alice")
	if len(public) != 1 || public[0].ID != f.sess.ID {
		t.Fatal("worker listed as a workspace", public)
	}
	usage := f.s.store.ListUsage(store.UsageFilter{Delegation: "only"})
	if len(usage) != 1 || usage[0].SessionID != f.sess.ID || usage[0].AccountID != "cx" || usage[0].CostMicroUSD != 1400 {
		t.Fatalf("%+v", usage)
	}
}
