package builtin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/pkg/extspi"
)

type recordingHost struct {
	mu      sync.Mutex
	logs    []string
	metrics map[string]float64
	state   string
}

func (h *recordingHost) ReportStatus(p extspi.StatusReport) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = p.State
	if p.Metrics != nil {
		h.metrics = p.Metrics
	}
}
func (h *recordingHost) Log(p extspi.LogParams) {
	h.mu.Lock()
	h.logs = append(h.logs, p.Message)
	h.mu.Unlock()
}
func (h *recordingHost) metric(name string) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.metrics[name]
}

func startWebhook(t *testing.T, config string, perms ...string) (*Webhook, *recordingHost, extspi.InitializeResult) {
	t.Helper()
	w, host := &Webhook{}, &recordingHost{}
	res, err := w.Initialize(context.Background(), extspi.InitializeParams{Config: json.RawMessage(config), Permissions: perms}, host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Shutdown(context.Background()) })
	return w, host, res
}

func waitMetric(t *testing.T, host *recordingHost, name string, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for host.metric(name) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s = %v, want %v", name, host.metric(name), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var (
	ref     = extspi.WorkspaceRef{ID: "s1", Name: "demo", Agent: "claude", Account: "relay", User: "alice"}
	failed  = extspi.EventParams{Hook: extspi.HookChatTurnEnded, ID: "evt_1", Turn: &extspi.TurnEvent{Workspace: ref, Turn: "t1", Status: extspi.TurnFailed, Code: "chat_failed", Model: "claude-opus-5", DurationMS: 192_000, CostMicroUSD: 420_000}}
	done    = extspi.EventParams{Hook: extspi.HookChatTurnEnded, ID: "evt_2", Turn: &extspi.TurnEvent{Workspace: ref, Turn: "t2", Status: extspi.TurnCompleted}}
	idleOff = extspi.EventParams{Hook: extspi.HookWorkspaceLifecycle, ID: "evt_3", Workspace: &extspi.WorkspaceEvent{Action: extspi.WorkspaceStopped, Reason: "idle", Workspace: ref}}
)

func TestWebhookFormatsAndFilters(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		mu.Lock()
		bodies = append(bodies, v)
		mu.Unlock()
		io.WriteString(rw, `{"code":0}`)
	}))
	defer srv.Close()

	// Plain JSON with the default filter: failed turns only, plus lifecycle.
	w, host, res := startWebhook(t, `{"url":"`+srv.URL+`"}`, extspi.PermWorkspaceEvents, extspi.PermChatTurns)
	if len(res.Hooks) != 2 {
		t.Fatalf("hooks: %v", res.Hooks)
	}
	w.Event(failed)
	w.Event(done)
	w.Event(idleOff)
	waitMetric(t, host, "sent", 2)
	mu.Lock()
	if len(bodies) != 2 {
		t.Fatalf("bodies: %+v", bodies)
	}
	var turn, life map[string]any
	for _, b := range bodies {
		if b["hook"] == extspi.HookChatTurnEnded {
			turn = b
		} else {
			life = b
		}
	}
	mu.Unlock()
	if turn["id"] != "evt_1" || !strings.Contains(turn["text"].(string), "alice 的空间「demo」对话失败 (chat_failed)") ||
		!strings.Contains(turn["text"].(string), "耗时 3m12s · 费用 $0.42") || turn["turn"].(map[string]any)["status"] != "failed" {
		t.Fatalf("turn body: %+v", turn)
	}
	if !strings.Contains(life["text"].(string), "已停止（空闲休眠）") {
		t.Fatalf("lifecycle body: %+v", life)
	}

	// A bot format in English; only the granted hook is subscribed.
	bodies = nil
	w2, host2, res2 := startWebhook(t, `{"url":"`+srv.URL+`","format":"feishu","turns":"all","language":"en"}`, extspi.PermChatTurns)
	if len(res2.Hooks) != 1 || res2.Hooks[0] != extspi.HookChatTurnEnded {
		t.Fatalf("hooks: %v", res2.Hooks)
	}
	w2.Event(done)
	waitMetric(t, host2, "sent", 1)
	mu.Lock()
	defer mu.Unlock()
	content, _ := bodies[0]["content"].(map[string]any)
	if bodies[0]["msg_type"] != "text" || !strings.Contains(content["text"].(string), `alice's workspace "demo": chat turn completed`) {
		t.Fatalf("feishu body: %+v", bodies[0])
	}
}

// Failures are counted and retried once for 5xx; the log never shows the URL,
// which usually carries the bot's secret.
func TestWebhookFailuresKeepTheURLSecret(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("key") == "bot-error" {
			io.WriteString(rw, `{"errcode":310000,"errmsg":"keywords not in content"}`)
			return
		}
		rw.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	w, host, _ := startWebhook(t, `{"url":"`+srv.URL+`/hook?key=secret-token","turns":"all"}`, extspi.PermChatTurns)
	w.Event(done)
	waitMetric(t, host, "failed", 1)
	if calls.Load() != 2 {
		t.Fatalf("5xx not retried once: %d calls", calls.Load())
	}
	w2, host2, _ := startWebhook(t, `{"url":"`+srv.URL+`/hook?key=bot-error","format":"dingtalk","turns":"all"}`, extspi.PermChatTurns)
	w2.Event(done)
	waitMetric(t, host2, "failed", 1)
	for _, h := range []*recordingHost{host, host2} {
		h.mu.Lock()
		for _, line := range h.logs {
			if strings.Contains(line, "secret-token") || strings.Contains(line, srv.URL) {
				t.Errorf("log leaks the URL: %s", line)
			}
		}
		h.mu.Unlock()
	}
	if !strings.Contains(strings.Join(host2.logs, "\n"), "errcode 310000") {
		t.Fatalf("bot error not reported: %v", host2.logs)
	}
}

func TestWebhookWithoutURL(t *testing.T) {
	w, host, _ := startWebhook(t, `{}`, extspi.PermChatTurns)
	w.Event(failed)
	if host.state != "degraded" || len(host.logs) != 1 {
		t.Fatalf("missing URL not reported: %s %v", host.state, host.logs)
	}
	if _, err := (&Webhook{}).Initialize(context.Background(), extspi.InitializeParams{Config: json.RawMessage(`{"url":"ftp://x"}`)}, &recordingHost{}); err == nil {
		t.Fatal("non-http URL accepted")
	}
}
