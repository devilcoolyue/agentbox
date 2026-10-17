package extspi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingHost struct {
	mu     sync.Mutex
	aborts []AbortParams
	status []StatusReport
	logs   []LogParams
}

func (h *recordingHost) Abort(_ context.Context, p AbortParams) (AbortResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.aborts = append(h.aborts, p)
	return AbortResult{Applied: true}, nil
}
func (h *recordingHost) ReportStatus(p StatusReport) {
	h.mu.Lock()
	h.status = append(h.status, p)
	h.mu.Unlock()
}
func (h *recordingHost) Log(p LogParams) { h.mu.Lock(); h.logs = append(h.logs, p); h.mu.Unlock() }

// echoHandler rejects requests whose path contains "deny" and aborts a stream
// from inside its delta handler, which must not deadlock the transport.
type echoHandler struct {
	BaseModelHandler
	host   ModelHost
	mu     sync.Mutex
	seqs   []int64
	closed []string
	config json.RawMessage
}

func (e *echoHandler) Initialize(_ context.Context, p InitializeParams, host Host) (InitializeResult, error) {
	e.host, e.config = host.(ModelHost), p.Config
	return InitializeResult{SPIVersion: Version, Hooks: []string{HookModelRequest, HookModelStream}}, nil
}

func (e *echoHandler) ModelRequest(_ context.Context, p ModelRequestParams) (RequestVerdict, error) {
	if strings.Contains(p.Path, "deny") {
		return RequestVerdict{Action: ActionReject, Code: "denied", Message: "no"}, nil
	}
	if strings.Contains(p.Path, "fail") {
		return RequestVerdict{}, errors.New("boom")
	}
	return RequestVerdict{Action: ActionAllow}, nil
}

func (e *echoHandler) StreamDelta(p StreamDeltaParams) {
	e.mu.Lock()
	e.seqs = append(e.seqs, p.Seq)
	e.mu.Unlock()
	if len(p.Deltas) > 0 && p.Deltas[0].Text == "stop" {
		res, err := e.host.Abort(context.Background(), AbortParams{Stream: p.Stream, Code: "loop", Message: "m", Detail: map[string]float64{"window": 4}})
		if err != nil || !res.Applied {
			panic("abort failed")
		}
		e.host.ReportStatus(StatusReport{State: "ok", Metrics: map[string]float64{"trips": 1}})
		e.host.Log(LogParams{Level: "info", Message: "tripped"})
	}
}

func (e *echoHandler) StreamClose(p StreamCloseParams) {
	e.mu.Lock()
	e.closed = append(e.closed, p.Reason)
	e.mu.Unlock()
}

func TestPipeRoundTrip(t *testing.T) {
	h := &echoHandler{}
	cl := Pipe(h)
	defer cl.Close()
	host := &recordingHost{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cl.Initialize(ctx, InitializeParams{SPIVersions: []string{Version}, Config: json.RawMessage(`{"window":4}`)}, host)
	if err != nil || res.SPIVersion != Version || len(res.Hooks) != 2 {
		t.Fatalf("initialize: %+v %v", res, err)
	}
	if string(h.config) != `{"window":4}` {
		t.Fatalf("config not delivered: %s", h.config)
	}
	if v, err := cl.ModelRequest(ctx, ModelRequestParams{Stream: "s1", Path: "/v1/deny"}); err != nil || v.Action != ActionReject || v.Code != "denied" {
		t.Fatalf("reject: %+v %v", v, err)
	}
	var rpcErr *Error
	if _, err := cl.ModelRequest(ctx, ModelRequestParams{Stream: "s1", Path: "/fail"}); !errors.As(err, &rpcErr) || rpcErr.Code != CodeInternalError {
		t.Fatalf("handler error not reported: %v", err)
	}
	if err := cl.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	cl.StreamOpen(StreamOpenParams{Stream: "s1", Protocol: "anthropic"})
	for i := int64(1); i <= 3; i++ {
		cl.StreamDelta(StreamDeltaParams{Stream: "s1", Seq: i, Deltas: []Delta{{Part: "0", Channel: "text", Text: "go"}}})
	}
	cl.StreamDelta(StreamDeltaParams{Stream: "s1", Seq: 4, Deltas: []Delta{{Part: "0", Channel: "text", Text: "stop"}}})
	cl.StreamClose(StreamCloseParams{Stream: "s1", Reason: CloseAborted})
	deadline := time.Now().Add(3 * time.Second)
	for {
		host.mu.Lock()
		done := len(host.aborts) == 1 && len(host.status) == 1 && len(host.logs) == 1
		host.mu.Unlock()
		h.mu.Lock()
		done = done && len(h.closed) == 1
		h.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("abort/status/log/close not delivered (deadlock?)")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.mu.Lock()
	if len(h.seqs) != 4 || h.seqs[0] != 1 || h.seqs[3] != 4 {
		t.Fatalf("notifications out of order: %v", h.seqs)
	}
	h.mu.Unlock()
	if host.aborts[0].Detail["window"] != 4 || host.aborts[0].Stream != "s1" {
		t.Fatalf("bad abort: %+v", host.aborts[0])
	}
	if err := cl.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cl.Done():
	case <-time.After(time.Second):
		t.Fatal("connection not closed after shutdown")
	}
}

func TestUnknownMethodAndOversizedMessage(t *testing.T) {
	hostR, extW := io.Pipe()
	extR, hostW := io.Pipe()
	go Serve(context.Background(), extR, extW, &echoHandler{})
	cl := NewClient(hostR, hostW)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var rpcErr *Error
	if err := cl.c.call(ctx, "no/such", struct{}{}, nil); !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("unknown method: %v", err)
	}
	// An oversized line from the host ends the extension's connection.
	go hostW.Write([]byte(`{"jsonrpc":"2.0","method":"log","params":"` + strings.Repeat("x", MaxMessageBytes) + `"}` + "\n"))
	select {
	case <-cl.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("oversized message did not end the connection")
	}
	if err := cl.Ping(ctx); err == nil {
		t.Fatal("call on closed connection succeeded")
	}
}

// plainHandler subscribes to no hooks and implements no hook interface.
type plainHandler struct{ BaseHandler }

func (plainHandler) Initialize(context.Context, InitializeParams, Host) (InitializeResult, error) {
	return InitializeResult{SPIVersion: Version}, nil
}

// The core SPI carries no hook methods: an extension without ModelHandler
// answers model calls with method not found and cannot abort streams.
func TestHandlerWithoutHookInterfaces(t *testing.T) {
	cl := Pipe(plainHandler{})
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cl.Initialize(ctx, InitializeParams{SPIVersions: []string{Version}}, logOnlyHost{}); err != nil {
		t.Fatal(err)
	}
	var rpcErr *Error
	if _, err := cl.ModelRequest(ctx, ModelRequestParams{Stream: "s1"}); !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("model/request on a plain handler: %v", err)
	}
	if _, err := cl.dispatch(ctx, MethodStreamAbort, json.RawMessage(`{"stream":"s1","code":"x","message":"m"}`), false); !errors.As(err, &rpcErr) || rpcErr.Code != CodePermissionDenied {
		t.Fatalf("abort through a host without model hooks: %v", err)
	}
}

type logOnlyHost struct{}

func (logOnlyHost) ReportStatus(StatusReport) {}
func (logOnlyHost) Log(LogParams)             {}

func TestHookRegistry(t *testing.T) {
	for _, h := range Hooks() {
		if !KnownPermission(h.Permission) || h.Group == "" || len(h.Bindings) == 0 {
			t.Errorf("incomplete hook spec: %+v", h)
		}
		if got, ok := LookupHook(h.Name); !ok || got.Permission != h.Permission {
			t.Errorf("lookup %s: %+v", h.Name, got)
		}
	}
	if _, ok := LookupHook("no.such"); ok {
		t.Error("unknown hook found")
	}
	// Callers cannot mutate the registry through returned specs.
	h, _ := LookupHook(HookModelStream)
	h.Bindings[0] = "mutated"
	if again, _ := LookupHook(HookModelStream); again.Bindings[0] == "mutated" {
		t.Error("registry shares its slices")
	}
}

func TestCallContextCancel(t *testing.T) {
	// The extension never answers: nothing reads hostW's other end.
	hostR, _ := io.Pipe()
	_, hostW := io.Pipe()
	cl := NewClient(hostR, writerFunc(func(p []byte) (int, error) { return len(p), nil }))
	_ = hostW
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := cl.ModelRequest(ctx, ModelRequestParams{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
