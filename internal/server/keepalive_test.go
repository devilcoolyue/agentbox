package server

import (
	"encoding/json"
	"errors"
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

	"agentbox/internal/agent"
	"agentbox/internal/chat"
	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
	"agentbox/internal/usage"
	"github.com/docker/docker/pkg/stdcopy"
)

const keepaliveWarm = `{"type":"system","subtype":"init","session_id":"C1"}
{"type":"stream_event","event":{"type":"message_start","message":{"id":"m-keep","model":"claude-opus-5","usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":46000,"cache_creation_input_tokens":80,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":80}}}}}
{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}}
{"type":"result","subtype":"success","is_error":false}
`

type keepaliveFixture struct {
	s       *Server
	sess    store.Session
	thread  string
	output  atomic.Value // CLI stdout
	mu      sync.Mutex
	execs   []string // argv of each keepalive run
	prompts []string
}

func newKeepaliveFixture(t *testing.T) *keepaliveFixture {
	t.Helper()
	s, sess := newTestServer(t)
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"data_dir": s.cfg.DataDir, "auth_token": "synthetic-test-token",
		"accounts": []config.Account{{ID: "shared", Type: config.AgentClaude}},
		"pricing":  map[string]any{"claude-opus-5": map[string]float64{"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 10}}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	if s.cfg, err = config.Load(path); err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateUser(store.User{Name: "alice", Role: store.RoleUser}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Grant("alice", 1000000, "seed", "", "admin"); err != nil {
		t.Fatal(err)
	}
	sess.Agent, sess.AccountID, sess.DefaultModel, sess.Status, sess.ContainerID = config.AgentClaude, "shared", "claude-opus-5", store.StatusRunning, "fixture"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	s.chat = newChatManager(s)
	if err := os.MkdirAll(s.homeDir(sess), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateThreads(sess); err != nil {
		t.Fatal(err)
	}
	tid, err := s.ensureActiveThread(sess)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.appendThreadEntry(sess, tid, logEntry{Kind: "chat_session", Text: "C1"}); err != nil {
		t.Fatal(err)
	}
	f := &keepaliveFixture{s: s, sess: sess, thread: tid}
	if err := s.store.CreateToken("keep-alice", "alice"); err != nil {
		t.Fatal(err)
	}
	f.setKeepalive(t, store.WorkspaceKeepalive{Enabled: true, Terminal: true})
	f.output.Store(keepaliveWarm)
	var ids atomic.Int32
	var commands sync.Map
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.45")
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/containers/fixture/exec"):
			var body struct{ Cmd []string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			id := fmt.Sprintf("exec-%d", ids.Add(1))
			commands.Store(id, strings.Join(body.Cmd, " "))
			writeJSON(w, 201, map[string]string{"Id": id})
		case strings.Contains(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/start"):
			parts := strings.Split(r.URL.Path, "/")
			cmd, _ := commands.Load(parts[len(parts)-2])
			var opts struct{ Detach bool }
			_ = json.NewDecoder(r.Body).Decode(&opts) // the start options precede stdin
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			prompt, _ := io.ReadAll(conn)
			f.mu.Lock()
			f.execs = append(f.execs, cmd.(string))
			f.prompts = append(f.prompts, string(prompt))
			f.mu.Unlock()
			_, _ = io.WriteString(stdcopy.NewStdWriter(conn, stdcopy.Stdout), f.output.Load().(string))
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
	t.Cleanup(func() { closeTestServer(t, s); engine.Close() })
	return f
}

func (f *keepaliveFixture) runs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.execs)
}

func (f *keepaliveFixture) setKeepalive(t *testing.T, k store.WorkspaceKeepalive) {
	t.Helper()
	if err := f.s.store.SetKeepalive(f.sess.ID, k); err != nil {
		t.Fatal(err)
	}
}

// api calls the workspace's keepalive endpoint as user.
func (f *keepaliveFixture) api(t *testing.T, method, suffix, user string, body any) (int, keepaliveView) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "/api/sessions/"+f.sess.ID+"/keepalive"+suffix, strings.NewReader(string(raw)))
	r.Header.Set("Authorization", "Bearer "+user)
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	var v keepaliveView
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, v
}

func (f *keepaliveFixture) plan() keepalivePlan {
	now := time.Now()
	return keepalivePlan{keepalivePoint: keepalivePoint{source: keepaliveChat, thread: f.thread, account: "shared", at: now,
		point: chat.CachePoint{Conversation: "C1", Model: "claude-opus-5", Effort: "high", Control: "effort", TaskTools: true,
			Prompt: usage.PromptCache{Prompt: 150000, Read: 149000, TTL: "1h"}}},
		until: now.Add(3 * time.Hour), next: now}
}

func (f *keepaliveFixture) terminalPlan() keepalivePlan {
	now := time.Now()
	return keepalivePlan{keepalivePoint: keepalivePoint{source: keepaliveTerminal, cwd: "/workspace/app", account: "shared", at: now,
		point: chat.CachePoint{Conversation: "T1", Model: "claude-opus-5", Effort: "xhigh", Control: "effort",
			Prompt: usage.PromptCache{Prompt: 150000, Read: 149000, TTL: "1h"}}},
		until: now.Add(3 * time.Hour), next: now}
}

// planned returns the slot's plan, if any.
func planned(s *Server, key keepaliveKey) *keepalivePlan {
	s.keepalive.mu.Lock()
	defer s.keepalive.mu.Unlock()
	return s.keepalive.plans[key]
}

func keepaliveRows(t *testing.T, s *Server) []store.UsageEvent {
	t.Helper()
	var out []store.UsageEvent
	for _, row := range s.store.ListUsage(store.UsageFilter{User: "alice"}) {
		if row.Kind != store.UsageKindKeepalive {
			t.Fatalf("unexpected usage kind %q", row.Kind)
		}
		out = append(out, row)
	}
	return out
}

func TestKeepCacheReadsTheCacheWithoutSavingAndChargesIt(t *testing.T) {
	f := newKeepaliveFixture(t)
	thread, err := os.ReadFile(f.s.threadPath(f.sess, f.thread))
	if err != nil {
		t.Fatal(err)
	}
	cache, again, err := f.s.keepCache(f.sess.ID, f.plan())
	if err != nil || !again || f.runs() != 1 || cache.Read != 46000 || cache.Prompt != 46082 {
		t.Fatalf("keepalive: again=%v %v runs=%d cache=%+v", again, err, f.runs(), cache)
	}
	cmd := f.execs[0]
	for _, want := range []string{"--resume C1", "--model claude-opus-5", "--effort high", "--permission-mode default",
		"--max-turns 1 --no-session-persistence", agent.KeepalivePidFile, "CLAUDE_CODE_ENABLE_TODO_TOOLS=1"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("missing %q: %s", want, cmd)
		}
	}
	if strings.Contains(cmd, "cd ") {
		t.Fatalf("a chat conversation runs in /workspace: %s", cmd)
	}
	if f.prompts[0] != agent.KeepalivePrompt {
		t.Fatalf("prompt %q", f.prompts[0])
	}
	rows := keepaliveRows(t, f.s)
	// 2×5 + 3×25 + 46000×0.5 + 80×10 µUSD
	if len(rows) != 1 || rows[0].CostMicroUSD != 23885 || rows[0].ThreadID != f.thread || rows[0].CacheReadTokens != 46000 {
		t.Fatalf("rows %+v", rows)
	}
	if q, _ := f.s.store.GetQuota("alice"); q.BalanceMicroUSD != 1000000-23885 {
		t.Fatalf("not charged: %+v", q)
	}
	if after, _ := os.ReadFile(f.s.threadPath(f.sess, f.thread)); string(after) != string(thread) {
		t.Fatal("keepalive wrote to the chat history")
	}

	// A cache that is already gone is paid for once, then left alone.
	cold := strings.Replace(keepaliveWarm, `"cache_read_input_tokens":46000,"cache_creation_input_tokens":80`, `"cache_read_input_tokens":0,"cache_creation_input_tokens":46080`, 1)
	f.output.Store(strings.Replace(cold, "m-keep", "m-cold", 1)) // messages are billed once per ID
	if _, again, err := f.s.keepCache(f.sess.ID, f.plan()); again || !errors.Is(err, errKeepaliveCold) || len(keepaliveRows(t, f.s)) != 2 {
		t.Fatalf("cold cache kept: again=%v %v", again, err)
	}
	// A CLI that sends nothing says why in the log.
	f.output.Store(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"No conversation found with session ID: C1"}` + "\n")
	_, again, err = f.s.keepCache(f.sess.ID, f.plan())
	if again || !errors.Is(err, errKeepaliveStopped) || !strings.Contains(err.Error(), "No conversation found") || len(keepaliveRows(t, f.s)) != 2 {
		t.Fatalf("a CLI that sent nothing: again=%v %v", again, err)
	}
	f.output.Store(keepaliveWarm)
}

// A terminal session is resumed where its CLI ran, and charged the same way.
func TestKeepCacheTerminalSession(t *testing.T) {
	f := newKeepaliveFixture(t)
	if _, again, err := f.s.keepCache(f.sess.ID, f.terminalPlan()); err != nil || !again || f.runs() != 1 {
		t.Fatalf("keepalive: again=%v %v runs=%d", again, err, f.runs())
	}
	for _, want := range []string{"cd '/workspace/app' || exit 1; ", "--resume T1", "--effort xhigh", "--no-session-persistence"} {
		if !strings.Contains(f.execs[0], want) {
			t.Fatalf("missing %q: %s", want, f.execs[0])
		}
	}
	if rows := keepaliveRows(t, f.s); len(rows) != 1 || rows[0].ThreadID != "" || rows[0].CostMicroUSD != 23885 {
		t.Fatalf("rows %+v", rows)
	}
}

func TestKeepCacheLeavesWhatItMustNotTouch(t *testing.T) {
	f := newKeepaliveFixture(t)
	skip := func(name string, plan keepalivePlan) {
		t.Helper()
		if _, again, err := f.s.keepCache(f.sess.ID, plan); again || !errors.Is(err, errKeepaliveStopped) || f.runs() != 0 {
			t.Fatalf("%s: again=%v %v runs=%d", name, again, err, f.runs())
		}
	}
	other := f.plan()
	other.point.Conversation = "C0" // the thread moved on (/clear, deleted)
	skip("other conversation", other)
	other = f.plan()
	other.account = "previous" // the workspace switched accounts
	skip("other account", other)

	room := f.s.chat.room(f.sess.ID)
	if err := room.begin(); err != nil {
		t.Fatal(err)
	}
	skip("running turn", f.plan())
	room.end()

	f.setKeepalive(t, store.WorkspaceKeepalive{Enabled: true})
	skip("terminal sessions not kept", f.terminalPlan())
	f.setKeepalive(t, store.WorkspaceKeepalive{})
	skip("turned off", f.plan())
	f.setKeepalive(t, store.WorkspaceKeepalive{Enabled: true, Terminal: true})

	if _, err := f.s.store.Update(f.sess.ID, func(x *store.Session) { x.Status = store.StatusStopped; x.StopReason = "idle" }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.Update(f.sess.ID, func(x *store.Session) { x.StopReason = "" }); err != nil {
		t.Fatal(err)
	}
	skip("stopped by the user", f.plan())
	skip("terminal stopped by the user", f.terminalPlan())
	if _, err := f.s.store.Update(f.sess.ID, func(x *store.Session) { x.Status = store.StatusRunning }); err != nil {
		t.Fatal(err)
	}

	if _, err := f.s.store.Grant("alice", -1000000, "drain", "", "admin"); err != nil {
		t.Fatal(err)
	}
	skip("balance exhausted", f.plan())
}

// Plans follow the workspace's own setting; the latest point waits for it.
func TestKeepalivePlansFollowTheWorkspaceSetting(t *testing.T) {
	f := newKeepaliveFixture(t)
	s, id, key := f.s, f.sess.ID, chatSlot(f.sess.ID)
	f.setKeepalive(t, store.WorkspaceKeepalive{})
	turn := func(status string, prompt usage.PromptCache) chat.TurnOutcome {
		return chat.TurnOutcome{SessionID: id, ThreadID: f.thread, Status: status,
			Cache: &chat.CachePoint{Conversation: "C1", Model: "claude-opus-5", Prompt: prompt}}
	}
	warm := usage.PromptCache{Prompt: 150000, Read: 149000, TTL: "1h"}
	s.noteChatCache(turn(chat.OutcomeCompleted, warm))
	if planned(s, key) != nil {
		t.Fatal("planned while the workspace keeps nothing")
	}
	code, v := f.api(t, "GET", "", "keep-alice", nil)
	// 150000 × 0.5 to read, × 10 to write again
	if code != 200 || !v.Available || v.Enabled || v.Chat == nil || v.Chat.PromptTokens != 150000 || v.Chat.KeepMicroUSD != 75000 || v.Chat.RewriteMicroUSD != 1500000 {
		t.Fatalf("view %d %+v %+v", code, v, v.Chat)
	}
	// Turning it on plans the conversation that just ended.
	code, v = f.api(t, "PUT", "", "keep-alice", map[string]any{"enabled": true})
	p := planned(s, key)
	if code != 200 || p == nil || len(v.Plans) != 1 || v.Plans[0].ID != keepaliveChat || v.Plans[0].MaxRuns != 3 {
		t.Fatalf("enabling: %d %+v %+v", code, v, p)
	}
	if d := time.Until(p.next); d < 49*time.Minute || d > 50*time.Minute || time.Until(p.until) < 2*time.Hour+59*time.Minute {
		t.Fatalf("plan times %+v", p)
	}
	for name, o := range map[string]chat.TurnOutcome{
		"five-minute cache": turn(chat.OutcomeCompleted, usage.PromptCache{Prompt: 150000, TTL: "5m"}),
		"small prompt":      turn(chat.OutcomeCompleted, usage.PromptCache{Prompt: 50000, TTL: "1h"}),
		"failed turn":       turn(chat.OutcomeFailed, warm),
		"no cache point":    {SessionID: id, ThreadID: f.thread, Status: chat.OutcomeCompleted},
	} {
		s.noteChatCache(turn(chat.OutcomeCompleted, warm))
		s.noteChatCache(o) // the next turn replaces the plan, or ends it
		if planned(s, key) != nil {
			t.Fatalf("%s kept a plan", name)
		}
	}
	s.noteChatCache(turn(chat.OutcomeInterrupted, warm))
	p = planned(s, key)
	if p == nil {
		t.Fatal("an interrupted turn leaves its cache too")
	}
	s.noteChatCache(turn(chat.OutcomeCompleted, usage.PromptCache{})) // /context: no request
	if planned(s, key) != p {
		t.Fatal("a turn without a request moved the plan")
	}
	// The workspace's own threshold, then the administrator, drop the plan.
	if code, _ := f.api(t, "PUT", "", "keep-alice", map[string]any{"enabled": true, "min_context_tokens": 200000}); code != 200 || planned(s, key) != nil {
		t.Fatalf("threshold kept the plan: %d", code)
	}
	if code, _ := f.api(t, "PUT", "", "keep-alice", map[string]any{"enabled": true, "max_hours": 2}); code != 200 {
		t.Fatal(code)
	}
	if p := planned(s, key); p == nil || time.Until(p.until) > 2*time.Hour {
		t.Fatalf("max hours not applied: %+v", p)
	}
	if err := s.cfg.ApplySettings(config.SettingsPatch{CacheKeepalive: &config.CacheKeepaliveConfig{Disabled: true}}); err != nil {
		t.Fatal(err)
	}
	s.reconcileAllKeepalive()
	if planned(s, key) != nil {
		t.Fatal("disabled by the administrator, still planned")
	}
	if code, v := f.api(t, "GET", "", "keep-alice", nil); code != 200 || v.Available || v.Reason != "disabled" {
		t.Fatalf("view %d %+v", code, v)
	}
	if code, _ := f.api(t, "PUT", "", "keep-alice", map[string]any{"enabled": true}); code != 409 {
		t.Fatalf("enabled against the administrator: %d", code)
	}
}

func TestKeepaliveAPI(t *testing.T) {
	f := newKeepaliveFixture(t)
	if err := f.s.store.CreateUser(store.User{Name: "bob", Role: store.RoleUser}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.store.CreateToken("keep-bob", "bob"); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.api(t, "GET", "", "keep-bob", nil); code != 404 {
		t.Fatalf("another user read the setting: %d", code)
	}
	if code, _ := f.api(t, "PUT", "", "keep-bob", map[string]any{"enabled": false}); code != 404 {
		t.Fatalf("another user changed the setting: %d", code)
	}
	for _, bad := range []map[string]any{{"max_hours": 7}, {"max_hours": -1}, {"min_context_tokens": -5}, {"min_context_tokens": 3000000}} {
		if code, _ := f.api(t, "PUT", "", "keep-alice", bad); code != 400 {
			t.Fatalf("accepted %v: %d", bad, code)
		}
	}
	code, v := f.api(t, "PUT", "", "keep-alice", map[string]any{"enabled": true, "terminal": true, "max_hours": 6, "min_context_tokens": 60000})
	if code != 200 || !v.Enabled || !v.Terminal || v.Effective != (keepaliveLimits{6, 60000}) || v.Defaults != (keepaliveLimits{3, 100000}) ||
		v.MaxHoursLimit != 6 || v.IntervalMinutes != 50 {
		t.Fatalf("saved %d %+v", code, v)
	}
	f.s.noteTerminalCache(f.sess, usage.TerminalCache{Conversation: "T1", Cwd: "/workspace/app", Model: "claude-opus-5",
		Prompt: usage.PromptCache{Prompt: 80000, TTL: "1h"}, At: time.Now()})
	if code, v := f.api(t, "POST", "/stop", "keep-alice", map[string]string{"id": "terminal:/workspace/app"}); code != 200 || len(v.Plans) != 0 {
		t.Fatalf("stop %d %+v", code, v)
	}
	if code, _ := f.api(t, "POST", "/stop", "keep-alice", map[string]string{"id": "other"}); code != 400 {
		t.Fatalf("stopped an unknown slot: %d", code)
	}
	// An API key account writes five-minute caches.
	env := map[string]string{"ANTHROPIC_API_KEY": "sk-fixture"}
	if _, err := f.s.cfg.UpdateAccount("shared", config.AccountPatch{Env: &env}); err != nil {
		t.Fatal(err)
	}
	if code, v := f.api(t, "GET", "", "keep-alice", nil); code != 200 || v.Available || v.Reason != "account" {
		t.Fatalf("api key account %d %+v", code, v)
	}
}

// The terminal scan plans the sessions it reads, one per working directory.
func TestTerminalSessionsPlanKeepalive(t *testing.T) {
	f := newKeepaliveFixture(t)
	s, key := f.s, terminalSlot(f.sess.ID, "/workspace/app")
	f.setKeepalive(t, store.WorkspaceKeepalive{Enabled: true})
	now := time.Now()
	session := func(conv string, at time.Time) usage.TerminalCache {
		return usage.TerminalCache{Conversation: conv, Cwd: "/workspace/app", Model: "claude-opus-5", Effort: "xhigh",
			Prompt: usage.PromptCache{Prompt: 120000, Read: 119000, TTL: "1h"}, At: at}
	}
	s.noteTerminalCache(f.sess, session("T1", now.Add(-10*time.Minute)))
	if planned(s, key) != nil {
		t.Fatal("terminal sessions planned while only the chat is kept")
	}
	if code, _ := f.api(t, "PUT", "", "keep-alice", map[string]any{"enabled": true, "terminal": true}); code != 200 {
		t.Fatal(code)
	}
	p := planned(s, key)
	if p == nil || p.cwd != "/workspace/app" || p.point.Conversation != "T1" || p.point.Control != "effort" || p.thread != "" {
		t.Fatalf("plan %+v", p)
	}
	if d := time.Until(p.next); d < 39*time.Minute || d > 40*time.Minute {
		t.Fatalf("not due 50 minutes after the request: %v", d)
	}
	s.noteTerminalCache(f.sess, session("T0", now.Add(-20*time.Minute))) // a rescan reports an older session
	if p := planned(s, key); p == nil || p.point.Conversation != "T1" {
		t.Fatalf("older session replaced the plan: %+v", p)
	}
	s.noteTerminalCache(f.sess, session("T2", now)) // a new session in the same directory
	if p := planned(s, key); p == nil || p.point.Conversation != "T2" {
		t.Fatalf("new session not planned: %+v", p)
	}
	other := session("T3", now)
	other.Cwd = "/workspace/other"
	s.noteTerminalCache(f.sess, other)
	if planned(s, terminalSlot(f.sess.ID, "/workspace/other")) == nil || planned(s, key) == nil {
		t.Fatal("each directory keeps its own plan")
	}
	odd := session("T5", now)
	odd.Cwd, odd.Effort = "/workspace/odd", "ultra"
	s.noteTerminalCache(f.sess, odd)
	if p := planned(s, terminalSlot(f.sess.ID, "/workspace/odd")); p == nil || p.point.Effort != "" || p.point.Control != "" {
		t.Fatalf("an effort the CLI does not take: %+v", p)
	}
	gone := session("T1", now)
	gone.Exited = true
	s.noteTerminalCache(f.sess, gone)
	if planned(s, key) == nil {
		t.Fatal("an older session's exit ended the plan")
	}
	gone.Conversation = "T2"
	s.noteTerminalCache(f.sess, gone)
	if planned(s, key) != nil {
		t.Fatal("the CLI left its session, still planned")
	}
	s.noteTerminalCache(f.sess, session("T4", now.Add(-2*time.Hour)))
	if planned(s, key) != nil {
		t.Fatal("planned a session whose cache is gone")
	}
}

func TestKeepalivePlanRunsAgainUntilItsDeadline(t *testing.T) {
	f := newKeepaliveFixture(t)
	s, key := f.s, chatSlot(f.sess.ID)
	plan := f.plan()
	s.keepalive.note(key, plan.keepalivePoint)
	s.keepalive.mu.Lock()
	s.keepalive.armLocked(s, key, &plan)
	first := plan.gen // the timer is due at once and rearms the plan
	s.keepalive.mu.Unlock()
	waitFor(t, func() bool {
		s.keepalive.mu.Lock()
		defer s.keepalive.mu.Unlock()
		p := s.keepalive.plans[key]
		return f.runs() == 1 && p != nil && p.gen != first
	})
	// Rescheduled for another round, the cache's hour counted from the read.
	p := planned(s, key)
	if p.runs != 1 || p.last.Read != 46000 || time.Until(p.next) < 49*time.Minute || p.warm.IsZero() {
		t.Fatalf("rescheduled %+v", p)
	}
	if pt, _ := s.keepalive.point(key); pt.warm.IsZero() {
		t.Fatal("the read did not warm the point")
	}
	// A stale run never touches a newer plan.
	s.runKeepalive(key, first)
	if f.runs() != 1 {
		t.Fatal("a stale timer ran")
	}
	// Past the deadline the last run ends the plan.
	last := f.plan()
	last.until = time.Now().Add(10 * time.Minute)
	s.keepalive.mu.Lock()
	s.keepalive.armLocked(s, key, &last)
	s.keepalive.mu.Unlock()
	waitFor(t, func() bool { return f.runs() == 2 && planned(s, key) == nil })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A web chat turn plans the keepalive of the conversation it ran in.
func TestChatTurnPlansKeepalive(t *testing.T) {
	output := `{"type":"system","subtype":"init","session_id":"provider-session"}
{"type":"stream_event","session_id":"provider-session","event":{"type":"message_start","message":{"id":"turn-1","model":"claude-opus-5","usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":148000,"cache_creation_input_tokens":2000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":2000}}}}}
{"type":"stream_event","session_id":"provider-session","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"provider-session","result":"done"}
`
	f := newAgentReceiptFixture(t, config.AgentClaude, output, false)
	if err := f.s.store.SetKeepalive(f.sess.ID, store.WorkspaceKeepalive{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	f.unblock()
	id := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
	if c := f.wait(t, id); c.State != store.ChatCompleted {
		t.Fatal(c)
	}
	p := planned(f.s, chatSlot(f.sess.ID))
	if p == nil {
		t.Fatal("no keepalive planned")
	}
	if p.thread != f.input.ThreadID || p.point.Conversation != "provider-session" || p.point.Model != "claude-opus-5" ||
		p.point.Prompt != (usage.PromptCache{Prompt: 150002, Read: 148000, TTL: "1h"}) {
		t.Fatalf("plan %+v", p)
	}
	f.s.keepalive.forget(chatSlot(f.sess.ID))
}

// The terminal usage scan reaches the plan: a transcript written in the
// workspace home is planned once the workspace keeps terminal sessions.
func TestTerminalScanPlansKeepalive(t *testing.T) {
	f := newKeepaliveFixture(t)
	s := f.s
	dir := filepath.Join(s.homeDir(f.sess), ".claude", "projects", "-workspace-app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano)
	transcript := `{"type":"user","entrypoint":"cli","uuid":"u1","sessionId":"T1","cwd":"/workspace/app","timestamp":"` + at + `","message":{"content":"hi"}}
{"type":"assistant","entrypoint":"cli","uuid":"a1","sessionId":"T1","cwd":"/workspace/app","requestId":"req_1","effort":"xhigh","timestamp":"` + at + `","message":{"id":"msg_1","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":40,"cache_read_input_tokens":150000,"cache_creation_input_tokens":2000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":2000}}}}
`
	if err := os.WriteFile(filepath.Join(dir, "T1.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	s.usageService().Scan()
	p := planned(s, terminalSlot(f.sess.ID, "/workspace/app"))
	if p == nil || p.point.Conversation != "T1" || p.point.Prompt.Prompt != 152002 {
		t.Fatalf("scan did not plan the terminal session: %+v", p)
	}
}

// Production's order: the startup scan sees the session while the workspace
// keeps nothing; the setting is saved without a reconcile; the next requests
// arrive through incremental scans and plan it.
func TestTerminalScanPlansAfterLaterEnable(t *testing.T) {
	f := newKeepaliveFixture(t)
	s := f.s
	f.setKeepalive(t, store.WorkspaceKeepalive{})
	dir := filepath.Join(s.homeDir(f.sess), ".claude", "projects", "-workspace-app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	request := func(id string, at time.Time) string {
		ts := at.UTC().Format(time.RFC3339Nano)
		return `{"type":"user","entrypoint":"cli","uuid":"u-` + id + `","sessionId":"T1","cwd":"/workspace/app","timestamp":"` + ts + `","message":{"content":"hi"}}
{"type":"assistant","entrypoint":"cli","uuid":"a-` + id + `","sessionId":"T1","cwd":"/workspace/app","requestId":"req_` + id + `","effort":"xhigh","timestamp":"` + ts + `","message":{"id":"msg_` + id + `","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":40,"cache_read_input_tokens":150000,"cache_creation_input_tokens":2000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":2000}}}}
`
	}
	path := filepath.Join(dir, "T1.jsonl")
	if err := os.WriteFile(path, []byte(request("1", time.Now().Add(-3*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	s.usageService().Scan()
	key := terminalSlot(f.sess.ID, "/workspace/app")
	if planned(s, key) != nil {
		t.Fatal("planned while off")
	}
	f.setKeepalive(t, store.WorkspaceKeepalive{Enabled: true, Terminal: true})
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(request("2", time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	file.Close()
	s.usageService().Scan()
	if p := planned(s, key); p == nil || time.Until(p.next) < 48*time.Minute {
		t.Fatalf("the next request did not plan the session: %+v", p)
	}
}

// The points outlive the process: a restart plans again what was kept, from
// when its cache was last read and with the runs so far, and a terminal
// session kept past its hour comes back although its last request is older.
func TestKeepalivePointsSurviveARestart(t *testing.T) {
	f := newKeepaliveFixture(t)
	s := f.s
	path := filepath.Join(t.TempDir(), keepaliveFile)
	s.restoreKeepalive(path) // nothing saved yet
	now := time.Now()
	keptKey, recentKey, goneKey := terminalSlot(f.sess.ID, "/workspace/app"), terminalSlot(f.sess.ID, "/workspace/recent"), terminalSlot(f.sess.ID, "/workspace/gone")
	point := f.plan().point
	s.noteChatCache(chat.TurnOutcome{SessionID: f.sess.ID, ThreadID: f.thread, Status: chat.OutcomeCompleted, Cache: &point})
	kept := f.terminalPlan().keepalivePoint
	kept.at, kept.warm = now.Add(-2*time.Hour), now.Add(-10*time.Minute)
	s.keepalive.note(keptKey, kept)
	s.planKeepalive(f.sess, keptKey)
	s.keepalive.mu.Lock()
	s.keepalive.plans[keptKey].runs, s.keepalive.plans[keptKey].last = 2, usage.PromptCache{Prompt: 150000, Read: 149500, TTL: "1h"}
	s.keepalive.mu.Unlock()
	recent := usage.TerminalCache{Conversation: "T2", Cwd: "/workspace/recent", Model: "claude-opus-5",
		Prompt: usage.PromptCache{Prompt: 120000, Read: 119000, TTL: "1h"}, At: now.Add(-20 * time.Minute)}
	s.noteTerminalCache(f.sess, recent)
	gone := f.terminalPlan().keepalivePoint
	gone.cwd, gone.at = "/workspace/gone", now.Add(-90*time.Minute)
	s.keepalive.note(goneKey, gone)
	s.keepalive.note(chatSlot("deleted-workspace"), f.plan().keepalivePoint)
	if err := s.keepalive.save(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved file %v %v", info, err)
	}
	before := map[keepaliveKey]keepalivePlan{}
	s.keepalive.mu.Lock()
	for key, p := range s.keepalive.plans {
		p.timer.Stop()
		before[key] = *p
	}
	s.keepalive.mu.Unlock()
	if len(before) != 3 {
		t.Fatalf("plans before the restart: %v", before)
	}

	s.keepalive = keepaliveState{} // the next process
	s.restoreKeepalive(path)
	for key, was := range before {
		p := planned(s, key)
		if p == nil {
			t.Fatalf("%s not planned after the restart", key)
		}
		if p.source != was.source || p.thread != was.thread || p.cwd != was.cwd || p.account != was.account || p.point != was.point ||
			!p.at.Equal(was.at) || !p.warm.Equal(was.warm) || !p.next.Equal(was.next) || !p.until.Equal(was.until) || p.runs != was.runs || p.last != was.last {
			t.Fatalf("%s restored as %+v, was %+v", key, p, was)
		}
	}
	if _, ok := s.keepalive.point(goneKey); ok {
		t.Fatal("restored a point whose cache is gone")
	}
	if _, ok := s.keepalive.point(chatSlot("deleted-workspace")); ok {
		t.Fatal("restored a deleted workspace's point")
	}
	// The startup scan reports the recent session again: the plan stands.
	gen := planned(s, recentKey).gen
	s.noteTerminalCache(f.sess, recent)
	if p := planned(s, recentKey); p == nil || p.gen != gen {
		t.Fatalf("the scan's report replanned the restored session: %+v", p)
	}
}

// Close interrupts running turns, which then fail; such a turn keeps its
// cache like one the user stopped, and the last save on the way out has it,
// while a turn that really failed still ends the plan.
func TestShutdownKeepsTheCacheOfTheTurnItInterrupts(t *testing.T) {
	f := newKeepaliveFixture(t)
	s, key := f.s, chatSlot(f.sess.ID)
	path := filepath.Join(t.TempDir(), keepaliveFile)
	s.restoreKeepalive(path)
	point := f.plan().point
	failed := chat.TurnOutcome{SessionID: f.sess.ID, ThreadID: f.thread, Status: chat.OutcomeFailed, Cache: &point}
	s.noteChatCache(failed)
	if _, ok := s.keepalive.point(key); ok {
		t.Fatal("a failed turn left its point")
	}
	room := s.chat.room(f.sess.ID)
	if !room.tryBegin() {
		t.Fatal("cannot begin fixture turn")
	}
	interrupted := make(chan struct{})
	room.setStop(func() { close(interrupted) })
	s.spawn(func() {
		defer room.end()
		<-interrupted
		s.noteChatCache(failed)
		nothing := failed // a later turn that never reached its first request
		nothing.Cache = &chat.CachePoint{Conversation: "C1"}
		s.noteChatCache(nothing)
	})
	closeTestServer(t, s)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved keepaliveRecords
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Points) != 1 || saved.Points[0].Slot != keepaliveChat || saved.Points[0].Point != point {
		t.Fatalf("saved %s", raw)
	}
}
