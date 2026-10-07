package server

import (
	"context"
	"database/sql"
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
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gorilla/websocket"
)

type receiptFixture struct {
	s                        *Server
	sess                     store.Session
	u                        store.User
	input                    store.ChatRequestInput
	h                        http.Handler
	invocations, inspections atomic.Int32
	interruptions            atomic.Int32
	started                  chan struct{}
	appStarted               chan struct{}
	release                  chan struct{}
	releaseOnce              sync.Once
	output                   string
	corrupt                  bool
}

func newReceiptFixture(t *testing.T, output string, corrupt bool, holdHandshake ...bool) *receiptFixture {
	t.Helper()
	s, sess := newTestServer(t)
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"data_dir": s.cfg.DataDir, "auth_token": "synthetic-test-token", "accounts": []config.Account{{ID: "shared", Type: config.AgentCodex}}, "pricing": map[string]any{"codex": map[string]int{"input": 1}}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	s.cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob"} {
		if err = s.store.CreateUser(store.User{Name: name, Role: store.RoleUser}); err != nil {
			t.Fatal(err)
		}
		if err = s.store.CreateToken(name, name); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := s.store.GetUser("alice")
	sess.Agent, sess.AccountID, sess.DefaultModel, sess.Status, sess.ContainerID = config.AgentCodex, "shared", "gpt-5.5", store.StatusRunning, "fixture"
	if err = s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	s.chat = newChatManager(s)
	s.upgrader = websocket.Upgrader{}
	if err = os.MkdirAll(s.homeDir(sess), 0700); err != nil {
		t.Fatal(err)
	}
	if err = s.migrateThreads(sess); err != nil {
		t.Fatal(err)
	}
	tid, err := s.ensureActiveThread(sess)
	if err != nil {
		t.Fatal(err)
	}
	// A prior user turn prevents an unrelated title-generation call.
	if err = s.appendThreadEntry(sess, tid, logEntry{Kind: "user", Text: "synthetic previous turn"}); err != nil {
		t.Fatal(err)
	}
	if err = s.appendThreadEntry(sess, tid, logEntry{Kind: "title", Text: "Synthetic existing title"}); err != nil {
		t.Fatal(err)
	}
	f := &receiptFixture{s: s, sess: sess, u: u, started: make(chan struct{}), appStarted: make(chan struct{}), release: make(chan struct{}), output: output, corrupt: corrupt}
	f.input = store.ChatRequestInput{Scope: s.chatRequestScope(u), ThreadID: tid, Text: "synthetic prompt", Model: sess.DefaultModel}
	var commands sync.Map
	var execIDs atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.45")
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/containers/fixture/json"):
			_, _ = w.Write([]byte(`{"Id":"fixture","State":{"Running":true},"Mounts":[{"Destination":"/shared"}]}`))
		case strings.HasSuffix(r.URL.Path, "/containers/fixture/exec"):
			var body struct{ Cmd []string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			id := fmt.Sprintf("exec-%d", execIDs.Add(1))
			commands.Store(id, strings.Join(body.Cmd, " "))
			writeJSON(w, 201, map[string]string{"Id": id})
		case strings.Contains(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/start"):
			parts := strings.Split(r.URL.Path, "/")
			cmd, ok := commands.Load(parts[len(parts)-2])
			if !ok {
				t.Error("missing fixture exec")
				return
			}
			var opts struct{ Detach bool }
			_ = json.NewDecoder(r.Body).Decode(&opts)
			if opts.Detach {
				f.interruptions.Add(1)
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
			if strings.Contains(cmd.(string), "app-server") {
				close(f.appStarted)
				if len(holdHandshake) > 0 && holdHandshake[0] {
					select {
					case <-f.release:
					case <-t.Context().Done():
					}
				}
				return
			} // Safe pre-turn fallback.
			prompt, err := io.ReadAll(conn)
			if err != nil {
				return
			}
			if string(prompt) != f.input.Text {
				t.Errorf("unexpected prompt %q", prompt)
			}
			if f.invocations.Add(1) == 1 {
				close(f.started)
			}
			select {
			case <-f.release:
			case <-t.Context().Done():
				return
			}
			_, _ = io.WriteString(stdcopy.NewStdWriter(conn, stdcopy.Stdout), f.output)
			if f.corrupt {
				_, _ = conn.Write([]byte{9, 0, 0, 0, 0, 0, 0, 0})
			}
		case strings.Contains(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/json"):
			f.inspections.Add(1)
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
	s.dock, err = dockerx.New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.h = s.Handler()
	t.Cleanup(func() { f.unblock(); closeTestServer(t, s); engine.Close() })
	return f
}

func (f *receiptFixture) unblock() { f.releaseOnce.Do(func() { close(f.release) }) }
func (f *receiptFixture) request(method, suffix, user string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "/api/sessions/"+f.sess.ID+"/chat/"+suffix, strings.NewReader(string(raw)))
	r.Header.Set("Authorization", "Bearer "+user)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}
func receiptFrom(t *testing.T, w *httptest.ResponseRecorder) store.ChatRequest {
	t.Helper()
	var body struct {
		Version int               `json:"version"`
		Receipt store.ChatRequest `json:"receipt"`
	}
	if w.Code != 200 && w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Version != 1 || body.Receipt.RequestID == "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("invalid receipt %d %s", w.Code, w.Body.String())
	}
	return body.Receipt
}
func (f *receiptFixture) wait(t *testing.T, id string) store.ChatRequest {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		c, err := f.s.store.ChatRequest(f.u, f.sess.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if f.s.chat.room(f.sess.ID).state() == "idle" {
			return c
		}
		select {
		case <-deadline:
			t.Fatal("turn failed to settle", c)
		case <-ticker.C:
		}
	}
}

const receiptCompletedOutput = "{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":150,\"output_tokens\":1}}\n"

func TestChatReceiptHTTPConcurrentReplayExecutesAndChargesOnce(t *testing.T) {
	f := newReceiptFixture(t, receiptCompletedOutput, false)
	if _, err := f.s.store.Grant(f.u.Name, 1000, "synthetic", "", "admin"); err != nil {
		t.Fatal(err)
	}
	id := newOperationID()
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for range 12 {
		wg.Go(func() {
			w := f.request("PUT", "requests/"+id, "alice", f.input)
			if w.Code == 202 {
				accepted.Add(1)
			} else if w.Code != 200 {
				t.Errorf("replay %d %s", w.Code, w.Body.String())
			}
		})
	}
	wg.Wait()
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("runner not started")
	}
	if accepted.Load() != 1 || f.invocations.Load() != 1 {
		t.Fatal("duplicate scheduling", accepted.Load(), f.invocations.Load())
	}
	changed := f.input
	changed.Text += " changed"
	w := f.request("PUT", "requests/"+id, "alice", changed)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertProblem(t, w.Body.Bytes(), "chat_request_conflict")
	w = f.request("PUT", "requests/"+newOperationID(), "alice", f.input)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Losing the initial response is recovered by ID while execution continues.
	c := receiptFrom(t, f.request("GET", "requests/"+id, "alice", nil))
	if c.State != store.ChatRunning {
		t.Fatal(c)
	}
	f.unblock()
	c = f.wait(t, id)
	if c.State != store.ChatCompleted {
		t.Fatal("success not durably completed", c)
	}
	usage := f.s.store.ListUsage(store.UsageFilter{User: f.u.Name})
	quota, _ := f.s.store.GetQuota(f.u.Name)
	if len(usage) != 1 || usage[0].TurnID != c.TurnID || usage[0].CostMicroUSD != 150 || quota.BalanceMicroUSD != 850 {
		t.Fatal("settlement mismatch", usage, quota)
	}
	// Existing receipt reads/replays bypass account re-admission, but new work
	// is refused after revocation. No new model invocation or ledger row appears.
	if _, err := f.s.cfg.UpdateAccount("shared", config.AccountPatch{Access: &config.AccountAccess{Mode: "admin"}}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got := receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input)); got.State != store.ChatCompleted || got.TurnID != c.TurnID {
			t.Fatal(got)
		}
	}
	if w := f.request("PUT", "requests/"+newOperationID(), "alice", f.input); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.invocations.Load() != 1 || len(f.s.store.ListUsage(store.UsageFilter{})) != 1 {
		t.Fatal("replay executed or charged")
	}
	if w := f.request("GET", "requests/"+id, "bob", nil); w.Code != 404 || strings.Contains(w.Body.String(), f.input.Text) {
		t.Fatal("foreign receipt visible", w.Body.String())
	}
	// Thread deletion redacts the prompt but keeps the old ID unusable.
	w = f.request("DELETE", "threads/"+f.input.ThreadID, "alice", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	c, err := f.s.store.ChatRequest(f.u, f.sess.ID, id)
	if err != nil || c.State != store.ChatDeleted || c.Request != nil {
		t.Fatal(c, err)
	}
	w = f.request("PUT", "requests/"+id, "alice", f.input)
	if w.Code != 410 {
		t.Fatal("deleted ID re-admitted", w.Code, w.Body.String())
	}
}

func TestChatReceiptUnknownOutcomeBlocksLegacyAndThreadsUntilReview(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		corrupt      bool
	}{
		{"missing-terminal", "{\"type\":\"item.completed\"}\n", false},
		{"corrupt-output", receiptCompletedOutput, true},
		{"provider-failed", "{\"type\":\"turn.failed\"}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReceiptFixture(t, tc.output, tc.corrupt)
			id := newOperationID()
			receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
			f.unblock()
			c := f.wait(t, id)
			if c.State != store.ChatUncertain {
				t.Fatal("unknown execution guessed success/failure", c)
			}
			if tc.corrupt && f.inspections.Load() != 0 {
				t.Fatal("scanner error ignored and exit code checked")
			}
			for _, suffix := range []string{"threads", "threads/" + f.input.ThreadID + "/activate"} {
				w := f.request("POST", suffix, "alice", nil)
				if w.Code != 409 {
					t.Fatal("thread change escaped pending receipt", w.Code, w.Body.String())
				}
				assertProblem(t, w.Body.Bytes(), "chat_pending")
			}
			srv := httptest.NewServer(f.h)
			defer srv.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/sessions/"+f.sess.ID+"/chat", http.Header{"Authorization": []string{"Bearer alice"}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			var message map[string]any
			if err = conn.ReadJSON(&message); err != nil {
				t.Fatal(err)
			}
			if err = conn.WriteJSON(map[string]string{"type": "user_message", "text": "legacy duplicate", "model": f.input.Model}); err != nil {
				t.Fatal(err)
			}
			if err = conn.ReadJSON(&message); err != nil || message["code"] != "chat_pending" {
				t.Fatal(message, err)
			}
			w := f.request("POST", "requests/"+id, "alice", map[string]any{"action": "review", "revision": c.Revision - 1, "scope": f.input.Scope})
			if w.Code != 409 {
				t.Fatal("stale review accepted", w.Code)
			}
			c = receiptFrom(t, f.request("POST", "requests/"+id, "alice", map[string]any{"action": "review", "revision": c.Revision, "scope": f.input.Scope}))
			if c.State != store.ChatReviewed {
				t.Fatal(c)
			}
			if c = receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input)); c.State != store.ChatReviewed {
				t.Fatal(c)
			}
			if f.invocations.Load() != 1 {
				t.Fatal("unknown/reviewed ID repeated")
			}
		})
	}
}

func TestChatReceiptScopeAbandonAndOrphanRecovery(t *testing.T) {
	f := newReceiptFixture(t, receiptCompletedOutput, false)
	id := newOperationID()
	bad := f.input
	bad.Scope = "different-instance"
	w := f.request("PUT", "requests/"+id, "alice", bad)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	assertProblem(t, w.Body.Bytes(), "chat_scope_changed")
	receiptFrom(t, f.request("POST", "requests/"+id, "alice", map[string]any{"action": "abandon", "scope": f.input.Scope}))
	w = f.request("PUT", "requests/"+id, "alice", f.input)
	if w.Code != 410 {
		t.Fatal("late request crossed abandonment fence", w.Code)
	}
	id = newOperationID()
	// Simulate committed acceptance whose scheduler never ran (commit response
	// lost or process stopped). Query exposes uncertainty and does not dispatch.
	if _, fresh, err := f.s.store.AcceptChatRequest(f.u, f.sess.ID, id, f.input); err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	c := receiptFrom(t, f.request("GET", "requests/"+id, "alice", nil))
	if c.State != store.ChatUncertain || c.ErrorCode != "execution_incomplete" {
		t.Fatal(c)
	}
	w = f.request("GET", "requests?thread="+f.input.ThreadID, "alice", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"pending":{`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = f.request("GET", "requests?pending_only=1", "alice", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"requests":[]`) || !strings.Contains(w.Body.String(), `"pending":{`) {
		t.Fatal("polling should return pending without replaying historical prompts", w.Code, w.Body.String())
	}
	if f.invocations.Load() != 0 {
		t.Fatal("query dispatched abandoned/orphan work")
	}
	if _, err := f.s.store.AdvanceChatRequest(f.u, f.sess.ID, id, c.Revision, store.ChatReviewed, ""); err != nil {
		t.Fatal(err)
	}
	bad = f.input
	bad.ThreadID = newThreadID(time.Now().Add(time.Second))
	w = f.request("PUT", "requests/"+newOperationID(), "alice", bad)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	assertProblem(t, w.Body.Bytes(), "chat_thread_changed")
	bad = f.input
	bad.Attachments = []string{"/shared/.file/missing.txt"}
	w = f.request("PUT", "requests/"+newOperationID(), "alice", bad)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	assertProblem(t, w.Body.Bytes(), "chat_attachments_invalid")
	// The normal HTTP owner and bearer-only write boundary also covers this API.
	r := httptest.NewRequest("PUT", "/api/sessions/"+f.sess.ID+"/chat/requests/"+newOperationID()+"?token=alice", strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("query token accepted for a write", w.Code)
	}
}

func TestChatReceiptUserInterruptAndShutdownRemainDistinct(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			f := newReceiptFixture(t, "{\"type\":\"turn.failed\",\"status\":\"interrupted\"}\n", false)
			id := newOperationID()
			receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
			select {
			case <-f.started:
			case <-time.After(5 * time.Second):
				t.Fatal("not running")
			}
			room := f.s.chat.room(f.sess.ID)
			if shutdown {
				room.interruptContext(context.Background())
			} else {
				c := receiptFrom(t, f.request("GET", "requests/"+id, "alice", nil))
				for range 3 {
					receiptFrom(t, f.request("POST", "requests/"+id, "alice", map[string]any{"action": "interrupt", "revision": c.Revision, "scope": f.input.Scope}))
				}
				if f.interruptions.Load() != 1 {
					t.Fatal("replayed interrupt sent multiple signals", f.interruptions.Load())
				}
			}
			f.unblock()
			c := f.wait(t, id)
			want := store.ChatInterrupted
			if shutdown {
				want = store.ChatUncertain
			}
			if c.State != want {
				t.Fatal("interrupt origin lost", c)
			}
		})
	}
}

func TestChatReceiptAccountingFailureStaysUncertain(t *testing.T) {
	f := newReceiptFixture(t, receiptCompletedOutput, false)
	if _, err := f.s.store.Grant(f.u.Name, 1000, "synthetic", "", "admin"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(f.s.cfg.DataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER receipt_usage_failure BEFORE INSERT ON usage_events BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	id := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
	f.unblock()
	c := f.wait(t, id)
	if c.State != store.ChatUncertain || c.ErrorCode != "usage_write_failed" {
		t.Fatal("accounting failure became successful receipt", c)
	}
	q, _ := f.s.store.GetQuota(f.u.Name)
	if q.BalanceMicroUSD != 1000 || len(f.s.store.ListUsage(store.UsageFilter{})) != 0 {
		t.Fatal("usage/ledger transaction split", q)
	}
	for range 3 {
		receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
	}
	if f.invocations.Load() != 1 {
		t.Fatal("accounting error repeated execution")
	}
}

func TestChatReceiptInterruptDuringHandshakeDoesNotFallBackToExecution(t *testing.T) {
	f := newReceiptFixture(t, receiptCompletedOutput, false, true)
	id := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
	select {
	case <-f.appStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("handshake not started")
	}
	c := receiptFrom(t, f.request("GET", "requests/"+id, "alice", nil))
	receiptFrom(t, f.request("POST", "requests/"+id, "alice", map[string]any{"action": "interrupt", "revision": c.Revision, "scope": f.input.Scope}))
	f.unblock()
	c = f.wait(t, id)
	if f.invocations.Load() != 0 || c.State != store.ChatUncertain {
		t.Fatal("interrupted handshake launched a new exec", f.invocations.Load(), c)
	}
}

func TestChatReceiptStagePersistenceFailureNeverStartsRunner(t *testing.T) {
	f := newReceiptFixture(t, receiptCompletedOutput, false)
	db, err := sql.Open("sqlite", filepath.Join(f.s.cfg.DataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER receipt_stage_failure BEFORE UPDATE ON chat_requests BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	id := newOperationID()
	receiptFrom(t, f.request("PUT", "requests/"+id, "alice", f.input))
	c := f.wait(t, id)
	if c.State != store.ChatAccepted || f.invocations.Load() != 0 {
		t.Fatal("failed stage commit started work", c)
	}
	w := f.request("PUT", "requests/"+newOperationID(), "alice", f.input)
	if w.Code != 409 {
		t.Fatal("failed finalization released durable gate", w.Code, w.Body.String())
	}
	if _, err = db.Exec("DROP TRIGGER receipt_stage_failure"); err != nil {
		t.Fatal(err)
	}
	c = receiptFrom(t, f.request("GET", "requests/"+id, "alice", nil))
	if c.State != store.ChatUncertain || f.invocations.Load() != 0 {
		t.Fatal("recovery reran request", c)
	}
}

func TestChatReceiptProtectsAttachmentsUntilThreadDeletion(t *testing.T) {
	f := newReceiptFixture(t, receiptCompletedOutput, false)
	dir := filepath.Join(f.s.cfg.DataDir, "users", f.u.Name, "shared", ".file")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "receipt-only.txt")
	if err := os.WriteFile(path, []byte("synthetic attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	input := f.input
	input.Attachments = []string{"/shared/.file/receipt-only.txt"}
	id := newOperationID()
	c, _, err := f.s.store.AcceptChatRequest(f.u, f.sess.ID, id, input)
	if err != nil {
		t.Fatal(err)
	}
	f.s.cleanExpiredImages()
	if _, err = os.Stat(path); err != nil {
		t.Fatal("accepted attachment expired before transcript", err)
	}
	c, err = f.s.store.AdvanceChatRequest(f.u, f.sess.ID, id, c.Revision, store.ChatUncertain, "execution_incomplete")
	if err != nil {
		t.Fatal(err)
	}
	receiptFrom(t, f.request("POST", "requests/"+id, "alice", map[string]any{"action": "review", "revision": c.Revision, "scope": input.Scope}))
	w := f.request("DELETE", "threads/"+input.ThreadID, "alice", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.s.cleanExpiredImages()
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("deleted receipt kept attachment pinned", err)
	}
}

func TestChatReceiptHistorySyncRejectsLinks(t *testing.T) {
	f := newReceiptFixture(t, receiptCompletedOutput, false)
	path := f.s.threadPath(f.sess, f.input.ThreadID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	turn := &chatReceiptTurn{room: f.s.chat.room(f.sess.ID), owner: f.u, receipt: store.ChatRequest{ThreadID: f.input.ThreadID}}
	if err := turn.syncHistory(); err == nil {
		t.Fatal("receipt synced a linked transcript outside the root")
	}
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "preserved" {
		t.Fatal("outside file changed", err)
	}
}
