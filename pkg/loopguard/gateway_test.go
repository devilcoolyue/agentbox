package loopguard

import (
	"agentbox/pkg/modelgw"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func wire(v any) string { b, _ := json.Marshal(v); return "data: " + string(b) + "\n\n" }

type protocol = modelgw.Protocol

const (
	responses = modelgw.Responses
	chat      = modelgw.Chat
	anthropic = modelgw.Anthropic
	gemini    = modelgw.Gemini
)

// streamGuard decodes raw events with the gateway's decoder and feeds them to
// a default Monitor, exactly as a gateway stream does.
type streamGuard struct {
	protocol protocol
	decoder  *modelgw.Decoder
	monitor  *Monitor
}

func (g *streamGuard) observe(event []byte) *Detection {
	if g.decoder == nil {
		g.decoder, g.monitor = modelgw.NewDecoder(g.protocol), NewMonitor(0, 0)
	}
	return g.monitor.Push(g.decoder.Observe(event))
}

func delta(p protocol, text string) string {
	switch p {
	case responses:
		return wire(map[string]any{"type": "response.output_text.delta", "item_id": "item-1", "output_index": 0, "content_index": 0, "delta": text})
	case chat:
		return wire(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}})
	case anthropic:
		return wire(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": text}})
	case gemini:
		return wire(map[string]any{"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"parts": []any{map[string]any{"text": text, "thought": true}}}}}})
	}
	panic(p)
}

func TestGatewayStopsGenerationAllProtocols(t *testing.T) {
	for p, path := range map[protocol]string{responses: "/v1/responses", chat: "/v1/chat/completions", anthropic: "/v1/messages", gemini: "/v1beta/models/test:streamGenerateContent?alt=sse"} {
		t.Run(string(p), func(t *testing.T) {
			cancelled := make(chan struct{}, 2)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("X-Loop-Guard-Token") != "" || r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("forwarded credentials/encoding incorrectly")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, ": heartbeat\r\n\r\n")
				if p == responses {
					io.WriteString(w, wire(map[string]any{"type": "response.created", "sequence_number": 400, "response": map[string]any{"id": "resp-123", "model": "test", "created_at": 123}}))
				}
				// Deliberately split each SSE frame across HTTP writes.
				for i := 0; i < 270; i++ {
					e := delta(p, "Go.\n")
					io.WriteString(w, e[:7])
					w.(http.Flusher).Flush()
					io.WriteString(w, e[7:])
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
					cancelled <- struct{}{}
				case <-time.After(5 * time.Second):
					t.Error("upstream was not cancelled")
				}
			}))
			defer upstream.Close()
			g, err := New(Config{Upstream: upstream.URL, Token: "gateway-secret"})
			if err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(g)
			defer proxy.Close()
			request := func(body string) *http.Response {
				q, _ := http.NewRequest("POST", proxy.URL+path, strings.NewReader(body))
				q.Header.Set("Authorization", "Bearer provider-secret")
				q.Header.Set("X-Loop-Guard-Token", "gateway-secret")
				r, err := proxy.Client().Do(q)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			r := request(`{"model":"test","stream":true}`)
			b, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if r.StatusCode != 200 || !strings.Contains(string(b), ErrorCode) || !strings.HasPrefix(string(b), ": heartbeat\r\n\r\n") {
				t.Fatalf("missing stream error: %d %.180s", r.StatusCode, b)
			}
			if strings.Contains(string(b), "response.completed") || strings.Contains(string(b), "[DONE]") {
				t.Fatal("fabricated success")
			}
			if p == responses && (!strings.Contains(string(b), `"id":"resp-123"`) || !strings.Contains(string(b), "event: error") || !strings.Contains(string(b), `"sequence_number":401`)) {
				t.Fatal("lost response identity or invalid error sequence")
			}
			if p == responses && (!strings.Contains(string(b), "event: response.failed") || !strings.Contains(string(b), `"sequence_number":402`) || strings.Contains(string(b), `"output":[]`)) {
				t.Fatal("missing Codex terminal failure or fabricated empty output")
			}
			select {
			case <-cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream still running")
			}
			r = request(`{ "stream": true, "model": "test" }`)
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode != 422 || calls.Load() != 1 || r.Header.Get("Retry-After") != "" {
				t.Fatal("blind retry reached upstream")
			}
			r = request(`{"model":"test","stream":true,"input":"corrected"}`)
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode != 200 || calls.Load() != 2 {
				t.Fatal("new context blocked")
			}
		})
	}
}

func TestGatewayPreservesNormalWireAndUsage(t *testing.T) {
	content := ": ping\r\n\r\n" + delta(responses, "Working normally.\n") + wire(map[string]any{"type": "response.completed", "response": map[string]any{"usage": map[string]int{"output_tokens": 17}}})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/v1/responses" || r.URL.RawQuery != "tenant=fixture" {
			t.Errorf("wrong target: %s", r.URL)
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		io.WriteString(w, content)
	}))
	defer upstream.Close()
	g, _ := New(Config{Upstream: upstream.URL + "/prefix"})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses?tenant=fixture", strings.NewReader(`{}`)))
	if w.Body.String() != content {
		t.Fatal("normal SSE bytes or usage changed")
	}
}

func TestProtocolIsolationAndIgnoredFields(t *testing.T) {
	loop := strings.Repeat("Go.\n", 300)
	g := streamGuard{protocol: responses}
	for _, e := range []string{
		wire(map[string]any{"type": "response.function_call_arguments.delta", "delta": loop}),
		wire(map[string]any{"type": "response.output_text.done", "text": loop}),
	} {
		if g.observe([]byte(e)) != nil {
			t.Fatal("detected arguments or final snapshot")
		}
	}
	for _, p := range []protocol{chat, gemini} {
		g := streamGuard{protocol: p}
		for i := 0; i < 400; i++ {
			var e string
			if p == chat {
				e = wire(map[string]any{"choices": []any{map[string]any{"index": i % 2, "delta": map[string]any{"content": "Go.\n"}}}})
			} else {
				e = wire(map[string]any{"candidates": []any{map[string]any{"index": i % 2, "content": map[string]any{"parts": []any{map[string]any{"text": "Go.\n"}}}}}})
			}
			if g.observe([]byte(e)) != nil {
				t.Fatalf("merged independent %s candidates", p)
			}
		}
	}
	for i := 0; i < 400; i++ {
		typ := "response.output_text.delta"
		if i%2 == 0 {
			typ = "response.reasoning_summary_text.delta"
		}
		if g.observe([]byte(wire(map[string]any{"type": typ, "delta": "Go.\n", "item_id": "one"}))) != nil {
			t.Fatal("merged text and reasoning")
		}
	}
}

func TestGatewayAuthAndNonStreaming(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	g, _ := New(Config{Upstream: upstream.URL, Token: "secret"})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`)))
	if w.Code != 401 || calls.Load() != 0 {
		t.Fatal("unauthorized request forwarded")
	}
	q := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`))
	q.Header.Set("X-Loop-Guard-Token", "secret")
	w = httptest.NewRecorder()
	g.ServeHTTP(w, q)
	if w.Body.String() != `{"ok":true}` || w.Header().Get("X-Loop-Guard-Status") != "not-inspected" {
		t.Fatal("non-SSE incorrectly marked protected")
	}
}

func TestGatewayProviderBearerAuthentication(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-key" || r.Header.Get("X-Loop-Guard-Token") != "" {
			t.Error("provider auth was changed or gateway header leaked")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()
	g, err := New(Config{Upstream: upstream.URL, Token: "Bearer fixture-key", TokenHeader: "authorization"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "Bearer wrong-key", "Bearer fixture-key"} {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", value)
		r.Header.Set("X-Loop-Guard-Token", "untrusted")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		want := 401
		if value == "Bearer fixture-key" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("got %d, want %d", w.Code, want)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unauthorized request reached upstream")
	}
	if _, err := New(Config{Upstream: upstream.URL, TokenHeader: "Cookie"}); err == nil {
		t.Fatal("unsupported auth header accepted")
	}
}

func TestCooldownIsolationExpiryAndBound(t *testing.T) {
	c := NewCooldown(retryCooldown)
	for i := 0; i < 1100; i++ {
		var k [32]byte
		copy(k[:], fmt.Sprint(i))
		c.Block(k)
	}
	if c.Len() > 1024 {
		t.Fatal("unbounded cooldown")
	}
	var k [32]byte
	k[0] = 255
	if c.Blocked(k) {
		t.Fatal("unrelated request blocked")
	}
	c.blocked[k] = time.Now().Add(-time.Second)
	if c.Blocked(k) {
		t.Fatal("expired cooldown retained")
	}
	off := NewCooldown(0)
	off.Block(k)
	if off.Blocked(k) {
		t.Fatal("disabled cooldown blocked a request")
	}
}

func TestGatewayClientDisconnectCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": ready\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	g, _ := New(Config{Upstream: upstream.URL})
	proxy := httptest.NewServer(g)
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/v1/responses", strings.NewReader(`{}`))
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var first [1]byte
	if _, err := resp.Body.Read(first[:]); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("client disconnect left upstream generating")
	}
}

func TestDedicatedConnectionsCloseTLSOnLoop(t *testing.T) {
	closed := make(chan string, 8)
	requests := make(chan string, 8)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Like a real model endpoint, consume the request before generating;
		// net/http only starts its disconnect read after the body is consumed.
		_, _ = io.Copy(io.Discard, r.Body)
		requests <- r.RemoteAddr
		if r.ProtoMajor != 1 || !r.Close {
			t.Errorf("wanted dedicated HTTP/1.1 connection, got %s close=%t", r.Proto, r.Close)
		}
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"data":[]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, delta(responses, strings.Repeat("Go.\n", 256)))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
			t.Error("upstream did not observe cancellation")
		}
	}))
	upstream.EnableHTTP2 = true
	upstream.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- c.RemoteAddr().String()
		}
	}
	upstream.StartTLS()
	defer upstream.Close()
	transport := upstream.Client().Transport.(*http.Transport)
	g, err := New(Config{Upstream: upstream.URL, Transport: transport, DedicatedConnections: true})
	if err != nil {
		t.Fatal(err)
	}
	if transport.DisableKeepAlives {
		t.Fatal("modified caller's shared transport")
	}
	seen := make(map[string]bool)
	for _, path := range []string{"/v1/models", "/v1/models", "/v1/responses"} {
		method := "GET"
		if path == "/v1/responses" {
			method = "POST"
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		if w.Code != 200 {
			t.Fatalf("upstream request failed: %d %s", w.Code, w.Body.String())
		}
		if path == "/v1/responses" && !strings.Contains(w.Body.String(), ErrorCode) {
			t.Fatal("loop not interrupted")
		}
		var address string
		select {
		case address = <-requests:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream did not receive request")
		}
		if seen[address] {
			t.Fatal("upstream TCP connection was reused")
		}
		seen[address] = true
		select {
		case actual := <-closed:
			if actual != address {
				t.Fatal("closed the wrong connection")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("TCP/TLS connection stayed open after request ended")
		}
	}
}

func TestGatewayOversizeEventCancelsAndReportsFailure(t *testing.T) {
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: "+strings.Repeat("x", modelgw.MaxEventBytes))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	g, _ := New(Config{Upstream: upstream.URL})
	w := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`)).WithContext(ctx)
	g.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "loop_guard_stream_error") || strings.Contains(w.Body.String(), ErrorCode) {
		t.Fatal("oversize stream mislabeled or not reported")
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("oversize stream left upstream running")
	}
}

func TestProtocolChannelCoverageAndBlockReset(t *testing.T) {
	loop := strings.Repeat("Go.\n", 256)
	cases := []struct {
		p       protocol
		event   any
		channel string
	}{
		{responses, map[string]any{"type": "response.reasoning_text.delta", "delta": loop}, "reasoning"},
		{responses, map[string]any{"type": "response.reasoning_summary_text.delta", "delta": loop}, "reasoning"},
		{chat, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": loop}}}}, "reasoning"},
		{chat, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning": loop}}}}, "reasoning"},
		{anthropic, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": loop}}, "text"},
		{gemini, map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": loop}}}}}}, "text"},
	}
	for _, c := range cases {
		g := streamGuard{protocol: c.p}
		if hit := g.observe([]byte(wire(c.event))); hit == nil || hit.Channel != c.channel {
			t.Fatalf("missing %s %s detection", c.p, c.channel)
		}
	}
	g := streamGuard{protocol: anthropic}
	for i := 0; i < 3; i++ {
		if g.observe([]byte(delta(anthropic, strings.Repeat("Go.\n", 200)))) != nil {
			t.Fatal("combined closed blocks")
		}
		g.observe([]byte(wire(map[string]any{"type": "content_block_stop", "index": 0})))
	}
}
