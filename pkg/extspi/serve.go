package extspi

import (
	"context"
	"encoding/json"
	"io"
	"sync"
)

func decode(params json.RawMessage, v any) error {
	if len(params) == 0 {
		return &Error{Code: CodeInvalidParams, Message: "missing params"}
	}
	if err := json.Unmarshal(params, v); err != nil {
		return &Error{Code: CodeInvalidParams, Message: "invalid params"}
	}
	return nil
}

// remoteHost is the extension's view of a host across the transport.
type remoteHost struct{ c *conn }

func (h remoteHost) Abort(ctx context.Context, p AbortParams) (AbortResult, error) {
	var out AbortResult
	err := h.c.call(ctx, MethodStreamAbort, p, &out)
	return out, err
}
func (h remoteHost) ReportStatus(p StatusReport) { _ = h.c.notify(MethodStatusReport, p) }
func (h remoteHost) Log(p LogParams)             { _ = h.c.notify(MethodLog, p) }

// Serve runs an extension over r (host → extension) and w (extension →
// host), normally stdin and stdout. It returns after the host sends exit or
// closes the connection, or when ctx ends.
func Serve(ctx context.Context, r io.Reader, w io.Writer, h Handler) error {
	var c *conn
	exited := make(chan struct{})
	var once sync.Once
	c = newConn(w, func(ctx context.Context, method string, params json.RawMessage, notify bool) (any, error) {
		switch method {
		case MethodInitialize:
			var p InitializeParams
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			return h.Initialize(ctx, p, remoteHost{c})
		case MethodInitialized:
			return nil, nil
		case MethodPing:
			return struct{}{}, nil
		case MethodModelRequest:
			var p ModelRequestParams
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			return h.ModelRequest(ctx, p)
		case MethodStreamOpen:
			var p StreamOpenParams
			if decode(params, &p) == nil {
				h.StreamOpen(p)
			}
			return nil, nil
		case MethodStreamDelta:
			var p StreamDeltaParams
			if decode(params, &p) == nil {
				h.StreamDelta(p)
			}
			return nil, nil
		case MethodStreamClose:
			var p StreamCloseParams
			if decode(params, &p) == nil {
				h.StreamClose(p)
			}
			return nil, nil
		case MethodShutdown:
			return struct{}{}, h.Shutdown(ctx)
		case MethodExit:
			once.Do(func() { close(exited) })
			return nil, nil
		}
		if notify {
			return nil, nil
		}
		return nil, &Error{Code: CodeMethodNotFound, Message: "method not found"}
	})
	c.start(r)
	select {
	case <-exited:
	case <-c.done:
	case <-ctx.Done():
	}
	c.shutdown(ErrClosed)
	return nil
}

// Client is the host side of a transport. It implements Handler, so the host
// drives builtin and out-of-process extensions the same way.
type Client struct {
	c    *conn
	mu   sync.Mutex
	host Host
}

// NewClient connects to an extension reading its output from r and writing
// to w. Closing w (when it is an io.Closer) ends the extension's input.
func NewClient(r io.Reader, w io.Writer) *Client {
	cl := &Client{}
	cl.c = newConn(w, cl.dispatch)
	cl.c.start(r)
	return cl
}

func (cl *Client) dispatch(ctx context.Context, method string, params json.RawMessage, notify bool) (any, error) {
	cl.mu.Lock()
	host := cl.host
	cl.mu.Unlock()
	if host == nil {
		return nil, &Error{Code: CodeInvalidRequest, Message: "not initialized"}
	}
	switch method {
	case MethodStreamAbort:
		var p AbortParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return host.Abort(ctx, p)
	case MethodStatusReport:
		var p StatusReport
		if decode(params, &p) == nil {
			host.ReportStatus(p)
		}
		return nil, nil
	case MethodLog:
		var p LogParams
		if decode(params, &p) == nil {
			host.Log(p)
		}
		return nil, nil
	}
	if notify {
		return nil, nil
	}
	return nil, &Error{Code: CodeMethodNotFound, Message: "method not found"}
}

// Done is closed when the connection ends, for example because the extension
// exited or sent a malformed or oversized message.
func (cl *Client) Done() <-chan struct{} { return cl.c.done }

// Dropped counts notifications from the extension discarded because the
// dispatcher queue was full.
func (cl *Client) Dropped() int64 { return cl.c.dropped.Load() }

func (cl *Client) Initialize(ctx context.Context, p InitializeParams, host Host) (InitializeResult, error) {
	cl.mu.Lock()
	cl.host = host
	cl.mu.Unlock()
	var out InitializeResult
	if err := cl.c.call(ctx, MethodInitialize, p, &out); err != nil {
		return out, err
	}
	return out, cl.c.notify(MethodInitialized, struct{}{})
}

func (cl *Client) Ping(ctx context.Context) error {
	return cl.c.call(ctx, MethodPing, struct{}{}, nil)
}

func (cl *Client) ModelRequest(ctx context.Context, p ModelRequestParams) (RequestVerdict, error) {
	var out RequestVerdict
	err := cl.c.call(ctx, MethodModelRequest, p, &out)
	return out, err
}

func (cl *Client) StreamOpen(p StreamOpenParams)   { _ = cl.c.notify(MethodStreamOpen, p) }
func (cl *Client) StreamDelta(p StreamDeltaParams) { _ = cl.c.notify(MethodStreamDelta, p) }
func (cl *Client) StreamClose(p StreamCloseParams) { _ = cl.c.notify(MethodStreamClose, p) }

// Shutdown asks the extension to stop, sends exit and closes the connection.
func (cl *Client) Shutdown(ctx context.Context) error {
	err := cl.c.call(ctx, MethodShutdown, struct{}{}, nil)
	_ = cl.c.notify(MethodExit, struct{}{})
	cl.c.shutdown(ErrClosed)
	return err
}

// Close ends the connection without the shutdown handshake.
func (cl *Client) Close() { cl.c.shutdown(ErrClosed) }

// Pipe runs h behind a real NDJSON transport in this process and returns the
// host-side Client. Conformance tests use it to prove that a builtin
// extension's behavior does not depend on in-process calls.
func Pipe(h Handler) *Client {
	hostR, extW := io.Pipe()
	extR, hostW := io.Pipe()
	go func() {
		_ = Serve(context.Background(), extR, extW, h)
		_ = extW.Close()
		_ = extR.Close()
	}()
	return NewClient(hostR, hostW)
}
