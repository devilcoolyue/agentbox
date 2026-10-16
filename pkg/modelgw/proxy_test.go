package modelgw

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func wire(v any) string { b, _ := json.Marshal(v); return "data: " + string(b) + "\n\n" }

func textDelta(text string) string {
	return wire(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
}

func TestReadEventBoundaries(t *testing.T) {
	want := ": ping\r\ndata: {\r\ndata: \"x\": 1}\r\n\r\n"
	r := bufio.NewReaderSize(strings.NewReader(want+"data: tail"), 16)
	got, err := ReadEvent(r)
	if err != nil || string(got) != want {
		t.Fatalf("bad framing: %q %v", got, err)
	}
	got, err = ReadEvent(r)
	if err != io.EOF || string(got) != "data: tail" {
		t.Fatal("lost EOF tail")
	}
	_, err = ReadEvent(bufio.NewReader(strings.NewReader(strings.Repeat("x", MaxEventBytes+1))))
	if err == nil || err == io.EOF {
		t.Fatal("unbounded event")
	}
}

func TestFingerprintJSONNormalization(t *testing.T) {
	a := []byte(`{"input":[{"b":2,"a":1}],"n":9007199254740993}`)
	b := []byte(`{ "n":9007199254740993,"input":[{"a":1,"b":2}] }`)
	if string(FingerprintBody(a)) != string(FingerprintBody(b)) {
		t.Fatal("object ordering bypasses cooldown")
	}
	c := []byte(`{"input":[{"b":2,"a":1}],"n":9007199254740992}`)
	if string(FingerprintBody(a)) == string(FingerprintBody(c)) {
		t.Fatal("large integer rounded")
	}
	for _, raw := range []string{`not-json`, `{} {}`} {
		if string(FingerprintBody([]byte(raw))) != raw {
			t.Fatal("malformed input modified")
		}
	}
	r1 := httptest.NewRequest("POST", "/v1/messages", nil)
	r1.Header.Set("X-Api-Key", "one")
	r2 := httptest.NewRequest("POST", "/v1/messages", nil)
	r2.Header.Set("X-Api-Key", "two")
	if Fingerprint(r1, a) == Fingerprint(r2, a) || Fingerprint(r1, a) != Fingerprint(r1, b) {
		t.Fatal("fingerprint must separate keys and ignore key order")
	}
}

func TestDecoderPartsAndChannels(t *testing.T) {
	d := NewDecoder(Anthropic)
	deltas, ended := d.Observe([]byte(textDelta("hello")))
	if len(ended) != 0 || len(deltas) != 1 || deltas[0] != (Delta{Part: "0", Channel: "text", Text: "hello"}) {
		t.Fatalf("bad anthropic delta: %+v %v", deltas, ended)
	}
	_, ended = d.Observe([]byte(wire(map[string]any{"type": "content_block_stop", "index": 0})))
	if len(ended) != 1 || ended[0] != "0" {
		t.Fatal("block stop not reported")
	}
	c := NewDecoder(Chat)
	deltas, _ = c.Observe([]byte(wire(map[string]any{"choices": []any{map[string]any{"index": 1, "delta": map[string]any{"reasoning_content": "think", "content": "say"}}}})))
	if len(deltas) != 2 || deltas[0].Channel != "reasoning" || deltas[1].Channel != "text" || deltas[0].Part != "1" {
		t.Fatalf("bad chat deltas: %+v", deltas)
	}
	r := NewDecoder(Responses)
	deltas, _ = r.Observe([]byte(wire(map[string]any{"type": "response.function_call_arguments.delta", "delta": "x"})))
	if len(deltas) != 0 {
		t.Fatal("tool arguments decoded as text")
	}
}

type recordingStream struct {
	mu     sync.Mutex
	opened bool
	text   strings.Builder
	closes []Close
	hit    string // synchronous trip when the text contains this
}

func (s *recordingStream) Open() { s.mu.Lock(); s.opened = true; s.mu.Unlock() }
func (s *recordingStream) Observe(deltas []Delta, _ []string) *Verdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range deltas {
		s.text.WriteString(d.Text)
	}
	if s.hit != "" && strings.Contains(s.text.String(), s.hit) {
		return &Verdict{Code: "sync_trip", Message: "stopped"}
	}
	return nil
}
func (s *recordingStream) Close(c Close) { s.mu.Lock(); s.closes = append(s.closes, c); s.mu.Unlock() }

type inspectorFunc func(context.Context, Request) (Stream, *Verdict)

func (f inspectorFunc) Begin(ctx context.Context, r Request) (Stream, *Verdict) { return f(ctx, r) }

func newProxy(t *testing.T, upstream string, inspector Inspector) *Proxy {
	t.Helper()
	u, _ := url.Parse(upstream)
	p, err := New(Config{Route: func(r *http.Request) (Route, *Problem) {
		return Route{Upstream: u, Path: r.URL.Path, Transport: http.DefaultTransport, Inspector: inspector}, nil
	}, StatusHeader: "X-Test-Status", CodePrefix: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAsyncAbortCancelsUpstreamAndClosesOnce(t *testing.T) {
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, textDelta("first. "))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	stream := &recordingStream{}
	var abort func(Verdict) AbortOutcome
	ready := make(chan struct{})
	p := newProxy(t, upstream.URL, inspectorFunc(func(_ context.Context, r Request) (Stream, *Verdict) {
		abort = r.Abort
		close(ready)
		return stream, nil
	}))
	server := httptest.NewServer(p)
	defer server.Close()
	go func() {
		<-ready
		for {
			stream.mu.Lock()
			seen := stream.text.Len() > 0
			stream.mu.Unlock()
			if seen {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if got := abort(Verdict{Code: "async_trip", Message: "too repetitive"}); got != AbortApplied {
			t.Errorf("abort = %s", got)
		}
		if got := abort(Verdict{Code: "second", Message: "x"}); got != AbortSuperseded {
			t.Errorf("second abort = %s", got)
		}
	}()
	resp, err := http.Post(server.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("X-Test-Status") != "monitoring" || !strings.Contains(string(body), "first. ") || !strings.Contains(string(body), "async_trip") {
		t.Fatalf("unexpected stream: %s", body)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not cancelled")
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if !stream.opened || len(stream.closes) != 1 || stream.closes[0].Reason != Aborted || stream.closes[0].Verdict.Code != "async_trip" {
		t.Fatalf("bad close: %+v", stream.closes)
	}
	if got := abort(Verdict{Code: "late"}); got != AbortClosed {
		t.Fatalf("late abort = %s", got)
	}
}

func TestSyncVerdictWithholdsTrippingEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, textDelta("safe "))
		io.WriteString(w, textDelta("BAD"))
		io.WriteString(w, textDelta("after"))
	}))
	defer upstream.Close()
	stream := &recordingStream{hit: "BAD"}
	p := newProxy(t, upstream.URL, inspectorFunc(func(context.Context, Request) (Stream, *Verdict) { return stream, nil }))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`)))
	body := w.Body.String()
	if !strings.Contains(body, "safe ") || strings.Contains(body, "BAD") || strings.Contains(body, "after") || !strings.Contains(body, "sync_trip") {
		t.Fatalf("tripping event forwarded or failure missing: %s", body)
	}
	if len(stream.closes) != 1 || stream.closes[0].Reason != Aborted {
		t.Fatalf("bad close: %+v", stream.closes)
	}
}

func TestRejectAndNotInspected(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"bad"}`)
	}))
	defer upstream.Close()
	reject := true
	stream := &recordingStream{}
	p := newProxy(t, upstream.URL, inspectorFunc(func(context.Context, Request) (Stream, *Verdict) {
		if reject {
			return nil, &Verdict{Code: "cooldown", Message: "wait"}
		}
		return stream, nil
	}))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`)))
	if w.Code != http.StatusUnprocessableEntity || calls.Load() != 0 || w.Header().Get("Retry-After") != "" || !strings.Contains(w.Body.String(), `"code":"cooldown"`) {
		t.Fatalf("reject reached upstream or bad body: %d %s", w.Code, w.Body.String())
	}
	reject = false
	w = httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`)))
	if w.Code != http.StatusBadRequest || w.Body.String() != `{"error":"bad"}` || w.Header().Get("X-Test-Status") != "not-inspected" {
		t.Fatalf("error response changed: %d %s", w.Code, w.Body.String())
	}
	if stream.opened || len(stream.closes) != 1 || stream.closes[0].Reason != NotInspected {
		t.Fatalf("bad close: %+v", stream.closes)
	}
}

// A request forwarded without an inspector must still stream: buffering an SSE
// response until it ends would stall the CLI.
func TestUninspectedSSEIsFlushedPerEvent(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, textDelta("early"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer upstream.Close()
	defer close(release)
	server := httptest.NewServer(newProxy(t, upstream.URL, nil))
	defer server.Close()
	resp, err := http.Post(server.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if !strings.Contains(line, "early") {
			t.Fatalf("unexpected first line %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("uninspected SSE was buffered")
	}
	if resp.Header.Get("X-Test-Status") != "not-inspected" {
		t.Fatal("missing not-inspected status")
	}
}

func TestRouteProblemAndTransportGuards(t *testing.T) {
	p, _ := New(Config{Route: func(*http.Request) (Route, *Problem) {
		return Route{}, &Problem{Status: http.StatusForbidden, Code: "denied", Message: "no"}
	}})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", nil))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"denied"`) {
		t.Fatal("route problem not written")
	}
	u, _ := url.Parse("http://127.0.0.1:1")
	p, _ = New(Config{Route: func(r *http.Request) (Route, *Problem) {
		return Route{Upstream: u, Path: r.URL.Path, Transport: http.DefaultTransport}, nil
	}, CodePrefix: "gw"})
	r := httptest.NewRequest("GET", "/v1/messages", nil)
	r.Header.Set("Upgrade", "websocket")
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "gw_unsupported_transport") {
		t.Fatal("websocket upgrade accepted")
	}
}
