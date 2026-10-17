package extension

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/extension/builtin"
	"agentbox/pkg/extspi"
	"agentbox/pkg/modelgw"
)

func wire(v any) string { b, _ := json.Marshal(v); return "data: " + string(b) + "\n\n" }

func anthropicText(text string) string {
	return wire(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
}

// loopingUpstream streams the same short sentence until cancelled and counts
// requests that reached it.
func loopingUpstream(t *testing.T, calls *atomic.Int32, cancelled chan<- struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 2000; i++ {
			if _, err := io.WriteString(w, anthropicText("Go.\n")); err != nil {
				break
			}
			w.(http.Flusher).Flush()
			if r.Context().Err() != nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if r.Context().Err() != nil && cancelled != nil {
			select {
			case cancelled <- struct{}{}:
			default:
			}
		}
	}))
}

func gateway(t *testing.T, h *Host, upstream string, sc StreamContext) *httptest.Server {
	t.Helper()
	u, _ := url.Parse(upstream)
	p, err := modelgw.New(modelgw.Config{Route: func(r *http.Request) (modelgw.Route, *modelgw.Problem) {
		route := modelgw.Route{Upstream: u, Path: r.URL.Path, Transport: http.DefaultTransport}
		if in := h.Inspector(sc); in != nil {
			route.Inspector = in
		}
		return route, nil
	}, StatusHeader: "X-Agentbox-Extensions", CodePrefix: "model_gateway"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	return server
}

func loopSetting(accounts ...string) config.ExtensionSetting {
	return config.ExtensionSetting{Enabled: true, Revision: 1, Permissions: []string{extspi.PermRequestFingerprint, extspi.PermStreamText},
		Bindings: config.ExtensionBindings{Mode: "selected", Accounts: accounts}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func post(t *testing.T, server *httptest.Server, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", server.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "fixture-key")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestBuiltinManifestsAreValid(t *testing.T) {
	h, err := NewHost(Options{Builtins: builtin.All()})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := h.Package("agentbox.loop-guard")
	if !ok || p.Manifest.FailurePolicy != FailOpen || p.Manifest.Name.Pick("fr") != "Loop guard" {
		t.Fatalf("loop guard package: %+v", p.Manifest)
	}
}

func TestLoopGuardThroughGateway(t *testing.T) {
	for _, piped := range []bool{false, true} {
		name := "direct"
		if piped {
			name = "ndjson"
		}
		t.Run(name, func(t *testing.T) {
			var events []Event
			var eventsMu = make(chan struct{}, 1)
			eventsMu <- struct{}{}
			h, err := NewHost(Options{Builtins: builtin.All(), PipeBuiltins: piped, OnEvent: func(e Event) {
				<-eventsMu
				events = append(events, e)
				eventsMu <- struct{}{}
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close(context.Background())
			h.Apply(map[string]config.ExtensionSetting{"agentbox.loop-guard": loopSetting("relay-a")})
			waitFor(t, "running", func() bool { return h.Status("agentbox.loop-guard").State == "running" })

			var calls atomic.Int32
			cancelled := make(chan struct{}, 1)
			upstream := loopingUpstream(t, &calls, cancelled)
			defer upstream.Close()
			server := gateway(t, h, upstream.URL, StreamContext{Session: "sess1", Account: "relay-a", User: "alice", Agent: "claude"})

			resp, body := post(t, server, `{"model":"m","stream":true}`)
			if resp.Header.Get("X-Agentbox-Extensions") != "monitoring" || !strings.Contains(body, "model_output_loop") || !strings.Contains(body, "[agentbox.loop-guard]") {
				t.Fatalf("loop not interrupted: %s %.300s", resp.Header.Get("X-Agentbox-Extensions"), body)
			}
			select {
			case <-cancelled:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream kept generating")
			}
			resp, body = post(t, server, `{ "stream":true, "model":"m" }`)
			if resp.StatusCode != http.StatusUnprocessableEntity || calls.Load() != 1 || resp.Header.Get("Retry-After") != "" || !strings.Contains(body, "model_output_loop") {
				t.Fatalf("identical retry not rejected: %d calls=%d %s", resp.StatusCode, calls.Load(), body)
			}
			waitFor(t, "metrics", func() bool {
				m := h.Status("agentbox.loop-guard").Metrics
				return m["trips"] == 1 && m["cooldown_rejects"] == 1
			})
			got := h.Events("agentbox.loop-guard")
			if len(got) != 2 || got[0].Kind != "reject" || got[1].Kind != "abort" || got[1].Session != "sess1" || got[1].Detail["window"] != 256 {
				t.Fatalf("events: %+v", got)
			}
			if st := h.Status("agentbox.loop-guard"); st.ActiveStreams != 0 {
				t.Fatalf("stream leaked: %+v", st)
			}
		})
	}
}

func TestUnboundAccountAndDisable(t *testing.T) {
	h, _ := NewHost(Options{Builtins: builtin.All()})
	defer h.Close(context.Background())
	h.Apply(map[string]config.ExtensionSetting{"agentbox.loop-guard": loopSetting("relay-a")})
	if h.Inspector(StreamContext{Account: "other"}) != nil {
		t.Fatal("unbound account inspected")
	}
	relay, other := config.BindingTarget{Account: "relay-a"}, config.BindingTarget{Account: "other"}
	if len(h.Bound(relay, modelHooks...)) != 1 || len(h.Bound(other, modelHooks...)) != 0 {
		t.Fatal("bound list wrong")
	}
	if len(h.Bound(relay)) != 0 || !h.Uses(modelHooks...) || h.Uses() {
		t.Fatal("hook filter ignored")
	}
	off := loopSetting("relay-a")
	off.Enabled, off.Revision = false, 2
	h.Apply(map[string]config.ExtensionSetting{"agentbox.loop-guard": off})
	if h.Inspector(StreamContext{Account: "relay-a"}) != nil || h.Status("agentbox.loop-guard").State != "disabled" {
		t.Fatal("disabled extension still active")
	}
}

func TestNormalize(t *testing.T) {
	h, _ := NewHost(Options{Builtins: builtin.All()})
	s, err := h.Normalize("agentbox.loop-guard", config.ExtensionSetting{Config: json.RawMessage(`{"window":64}`), Permissions: []string{extspi.PermStreamText}})
	if err != nil {
		t.Fatal(err)
	}
	if string(s.Config) != `{"cooldown_seconds":30,"distinct_limit":24,"window":64}` || s.Version != "1.0.0" || s.Bindings.Mode != "none" {
		t.Fatalf("normalized: %s %+v", s.Config, s)
	}
	for _, bad := range []config.ExtensionSetting{
		{Config: json.RawMessage(`{"window":8}`)},
		{Config: json.RawMessage(`{"window":64.5}`)},
		{Config: json.RawMessage(`{"typo":1}`)},
		{Config: json.RawMessage(`[]`)},
		{Permissions: []string{extspi.PermContext}},
		{FailurePolicy: "maybe"},
		// Model hooks are bound by account only.
		{Bindings: config.ExtensionBindings{Mode: "selected", Users: []string{"alice"}}},
	} {
		if _, err := h.Normalize("agentbox.loop-guard", bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := h.Normalize("agentbox.nope", config.ExtensionSetting{}); err == nil {
		t.Fatal("unknown extension accepted")
	}
}

const testManifest = `{
  "id": "agentbox.test-ext", "version": "0.1.0", "spi": "1.0", "publisher": "test",
  "name": {"en": "Test"}, "description": {"en": "Test extension."},
  "runtime": {"type": "builtin"},
  "hooks": ["model.request", "model.stream"],
  "permissions": ["model.request.fingerprint", "model.stream.text", "model.context"],
  "bindings": ["account"], "failure_policy": "fail_open",
  "config_schema": {"type": "object"},
  "errors": [{"code": "test_stop", "message": {"en": "Stopped."}}]
}`

func TestManifestValidation(t *testing.T) {
	if _, err := ParseManifest([]byte(testManifest), true); err != nil {
		t.Fatal(err)
	}
	mutate := func(from, to string) []byte { return []byte(strings.Replace(testManifest, from, to, 1)) }
	// Error codes are only needed by extensions that reject or interrupt.
	if _, err := ParseManifest(mutate(`"errors": [{"code": "test_stop", "message": {"en": "Stopped."}}]`, `"errors": []`), true); err != nil {
		t.Fatalf("manifest without error codes: %v", err)
	}
	cases := map[string][]byte{
		"third party with official prefix": []byte(testManifest),
		"bad id":                           mutate(`"agentbox.test-ext"`, `"Agentbox.Test"`),
		"unknown field":                    mutate(`"publisher": "test",`, `"publisher": "test", "scripts": "x",`),
		"unknown hook":                     mutate(`"hooks": ["model.request", "model.stream"]`, `"hooks": ["model.request", "files.write"]`),
		"selector a hook cannot use":       mutate(`"bindings": ["account"]`, `"bindings": ["account", "user"]`),
		"unknown selector":                 mutate(`"bindings": ["account"]`, `"bindings": ["team"]`),
		"hook without permission":          mutate(`"permissions": ["model.request.fingerprint", "model.stream.text", "model.context"]`, `"permissions": ["model.stream.text"]`),
		"unsupported spi":                  mutate(`"spi": "1.0"`, `"spi": "2.0"`),
		"newer spi minor":                  mutate(`"spi": "1.0"`, `"spi": "1.9"`),
		"reserved error prefix":            mutate(`"test_stop"`, `"extension_stop"`),
		"control characters":               mutate(`"Test extension."`, `"Test\u0007"`),
		"unknown locale":                   mutate(`{"en": "Test"}`, `{"fr": "Test"}`),
		"container runtime unavailable":    mutate(`{"type": "builtin"}`, `{"type": "container", "image": "ghcr.io/x/y@sha256:`+strings.Repeat("a", 64)+`"}`),
	}
	for name, raw := range cases {
		builtinSource := name != "third party with official prefix"
		if _, err := ParseManifest(raw, builtinSource); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// panicky crashes on its first delta and can stall model/request.
type panicky struct {
	extspi.BaseModelHandler
	stall bool
}

func (p *panicky) Initialize(_ context.Context, _ extspi.InitializeParams, _ extspi.Host) (extspi.InitializeResult, error) {
	return extspi.InitializeResult{SPIVersion: extspi.Version, Hooks: []string{extspi.HookModelRequest, extspi.HookModelStream}}, nil
}
func (p *panicky) ModelRequest(ctx context.Context, _ extspi.ModelRequestParams) (extspi.RequestVerdict, error) {
	if p.stall {
		<-ctx.Done()
	}
	return extspi.RequestVerdict{Action: extspi.ActionAllow}, nil
}
func (p *panicky) StreamDelta(extspi.StreamDeltaParams) { panic("boom") }

func testHost(t *testing.T, h extspi.Handler) *Host {
	t.Helper()
	host, err := NewHost(Options{Builtins: []builtin.Entry{{Manifest: []byte(testManifest), New: func() extspi.Handler { return h }}},
		RequestTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Close(context.Background()) })
	return host
}

func testSetting(policy string) config.ExtensionSetting {
	return config.ExtensionSetting{Enabled: true, Revision: 1, FailurePolicy: policy,
		Permissions: []string{extspi.PermRequestFingerprint, extspi.PermStreamText},
		Bindings:    config.ExtensionBindings{Mode: "all"}}
}

func TestCrashFollowsFailurePolicy(t *testing.T) {
	for _, policy := range []string{FailOpen, FailClosed} {
		t.Run(policy, func(t *testing.T) {
			h := testHost(t, &panicky{})
			h.Apply(map[string]config.ExtensionSetting{"agentbox.test-ext": testSetting(policy)})
			waitFor(t, "running", func() bool { return h.Status("agentbox.test-ext").State == "running" })
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, anthropicText("first. "))
				w.(http.Flusher).Flush()
				time.Sleep(300 * time.Millisecond)
				io.WriteString(w, anthropicText("second. "))
			}))
			defer upstream.Close()
			server := gateway(t, h, upstream.URL, StreamContext{Account: "any"})
			_, body := post(t, server, `{}`)
			if policy == FailOpen && (!strings.Contains(body, "second. ") || strings.Contains(body, "extension_unavailable")) {
				t.Fatalf("fail_open interrupted the stream: %s", body)
			}
			if policy == FailClosed && (strings.Contains(body, "second. ") || !strings.Contains(body, "extension_unavailable")) {
				t.Fatalf("fail_closed kept streaming: %s", body)
			}
			st := h.Status("agentbox.test-ext")
			if st.State != "restarting" && st.State != "running" || st.Error == "" {
				t.Fatalf("crash not recorded: %+v", st)
			}
		})
	}
}

// noModel subscribes to model hooks without implementing ModelHandler.
type noModel struct{ extspi.BaseHandler }

func (noModel) Initialize(context.Context, extspi.InitializeParams, extspi.Host) (extspi.InitializeResult, error) {
	return extspi.InitializeResult{SPIVersion: extspi.Version, Hooks: []string{extspi.HookModelStream}}, nil
}

func TestSubscriptionNeedsHookInterface(t *testing.T) {
	h := testHost(t, noModel{})
	h.Apply(map[string]config.ExtensionSetting{"agentbox.test-ext": testSetting(FailOpen)})
	waitFor(t, "start failure", func() bool {
		st := h.Status("agentbox.test-ext")
		return st.State != "running" && strings.Contains(st.Error, "without implementing")
	})
	if h.Inspector(StreamContext{Account: "any"}) == nil {
		t.Fatal("inspector should still apply the failure policy while the extension is down")
	}
}

func TestUnavailableAndTimeoutPolicies(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicText("ok. "))
	}))
	defer upstream.Close()
	for _, policy := range []string{FailOpen, FailClosed} {
		t.Run(policy, func(t *testing.T) {
			h := testHost(t, &panicky{stall: true})
			h.Apply(map[string]config.ExtensionSetting{"agentbox.test-ext": testSetting(policy)})
			waitFor(t, "running", func() bool { return h.Status("agentbox.test-ext").State == "running" })
			server := gateway(t, h, upstream.URL, StreamContext{Account: "any"})
			resp, body := post(t, server, `{}`)
			if policy == FailOpen && (resp.StatusCode != 200 || !strings.Contains(body, "ok. ")) {
				t.Fatalf("timeout blocked fail_open: %d %s", resp.StatusCode, body)
			}
			if policy == FailClosed && (resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "extension_unavailable")) {
				t.Fatalf("timeout allowed fail_closed: %d %s", resp.StatusCode, body)
			}
		})
	}
}

// recordingModel records delta batches as delivered.
type recordingModel struct {
	extspi.BaseModelHandler
	got []extspi.StreamDeltaParams
}

func (r *recordingModel) Initialize(context.Context, extspi.InitializeParams, extspi.Host) (extspi.InitializeResult, error) {
	return extspi.InitializeResult{SPIVersion: extspi.Version}, nil
}
func (r *recordingModel) StreamDelta(p extspi.StreamDeltaParams) { r.got = append(r.got, p) }

// Queued delta batches of one stream coalesce up to the batch that ends a
// part, are numbered per delivered batch, never write into the batch slice
// shared with other instances, and count against the queue budget.
func TestStreamNoticesCoalesce(t *testing.T) {
	in := newInstance(&Host{}, &Package{}, config.ExtensionSetting{}, FailOpen)
	in.state = "running"
	part := &participant{in: in, stream: true}
	shared := []extspi.Delta{{Part: "0", Channel: "text", Text: "ab"}}
	for _, n := range []notice{
		&streamOpen{p: extspi.StreamOpenParams{Stream: "s"}},
		&streamDelta{part: part, stream: "s", deltas: shared},
		&streamDelta{part: part, stream: "s", deltas: []extspi.Delta{{Part: "0", Channel: "text", Text: "cd"}}, ended: []string{"0"}},
		&streamDelta{part: part, stream: "s", deltas: []extspi.Delta{{Part: "1", Channel: "text", Text: "ef"}}},
		&streamClose{p: extspi.StreamCloseParams{Stream: "s", Reason: extspi.CloseCompleted}},
	} {
		if in.enqueue(n) != queued {
			t.Fatal("enqueue refused")
		}
	}
	if in.queuedBytes != 6 {
		t.Fatalf("queued bytes %d", in.queuedBytes)
	}
	rec := &recordingModel{}
	for {
		item, ok := in.pop()
		if !ok {
			break
		}
		if err := in.deliver(rec, item); err != nil {
			t.Fatal(err)
		}
	}
	if in.queuedBytes != 0 || len(rec.got) != 2 {
		t.Fatalf("bytes %d, batches %+v", in.queuedBytes, rec.got)
	}
	first, second := rec.got[0], rec.got[1]
	if first.Seq != 1 || len(first.Deltas) != 2 || first.EndedParts[0] != "0" || second.Seq != 2 || second.Deltas[0].Text != "ef" {
		t.Fatalf("batches: %+v", rec.got)
	}
	if len(shared) != 1 || cap(shared) != 1 {
		t.Fatal("shared batch modified")
	}
	big := &streamDelta{part: part, stream: "s", deltas: []extspi.Delta{{Text: strings.Repeat("x", queueBudget+1)}}}
	if in.enqueue(big) != overBudget {
		t.Fatal("over-budget batch accepted")
	}
}

// eventRecorder is an event-only extension.
type eventRecorder struct {
	extspi.BaseHandler
	got chan extspi.EventParams
}

func (e *eventRecorder) Initialize(context.Context, extspi.InitializeParams, extspi.Host) (extspi.InitializeResult, error) {
	return extspi.InitializeResult{SPIVersion: extspi.Version, Hooks: []string{extspi.HookChatTurnEnded}}, nil
}
func (e *eventRecorder) Event(p extspi.EventParams) { e.got <- p }

const eventManifest = `{
  "id": "agentbox.test-events", "version": "0.1.0", "spi": "1.0", "publisher": "test",
  "name": {"en": "Events"}, "description": {"en": "Records events."},
  "runtime": {"type": "builtin"},
  "hooks": ["chat.turn.ended"], "permissions": ["chat.turns"],
  "bindings": ["account", "user"], "failure_policy": "fail_open",
  "config_schema": {"type": "object"}
}`

// Events reach a bound extension through the real NDJSON transport, only for
// targets its bindings select, and never reach the model inspector.
func TestEventsReachBoundExtensionsOverTransport(t *testing.T) {
	rec := &eventRecorder{got: make(chan extspi.EventParams, 4)}
	h, err := NewHost(Options{Builtins: []builtin.Entry{{Manifest: []byte(eventManifest), New: func() extspi.Handler { return rec }}}, PipeBuiltins: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(context.Background())
	h.Apply(map[string]config.ExtensionSetting{"agentbox.test-events": {Enabled: true, Revision: 1, Permissions: []string{extspi.PermChatTurns},
		Bindings: config.ExtensionBindings{Mode: "selected", Users: []string{"alice"}}}})
	waitFor(t, "running", func() bool { return h.Status("agentbox.test-events").State == "running" })
	if h.Inspector(StreamContext{Account: "relay", User: "alice"}) != nil || h.Uses(modelHooks...) {
		t.Fatal("an event extension joined the model hooks")
	}
	turn := &extspi.TurnEvent{Workspace: extspi.WorkspaceRef{ID: "s1", User: "bob"}, Turn: "t0", Status: extspi.TurnCompleted}
	h.Emit(extspi.HookChatTurnEnded, config.BindingTarget{User: "bob"}, extspi.EventParams{Turn: turn})
	h.Emit(extspi.HookWorkspaceLifecycle, config.BindingTarget{User: "alice"}, extspi.EventParams{Workspace: &extspi.WorkspaceEvent{Action: "started"}})
	turn = &extspi.TurnEvent{Workspace: extspi.WorkspaceRef{ID: "s2", User: "alice"}, Turn: "t1", Status: extspi.TurnFailed, Code: "chat_failed"}
	h.Emit(extspi.HookChatTurnEnded, config.BindingTarget{User: "alice"}, extspi.EventParams{Turn: turn})
	select {
	case p := <-rec.got:
		if p.Hook != extspi.HookChatTurnEnded || p.Turn == nil || p.Turn.Turn != "t1" || p.Turn.Code != "chat_failed" || !strings.HasPrefix(p.ID, "evt_") || p.Time.IsZero() {
			t.Fatalf("event: %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not delivered")
	}
	select {
	case p := <-rec.got:
		t.Fatalf("unbound or unsubscribed event delivered: %+v", p)
	case <-time.After(100 * time.Millisecond):
	}
}
