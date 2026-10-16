package extspi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned by calls on a connection that has ended.
var ErrClosed = errors.New("extspi: connection closed")

var errTooLarge = errors.New("extspi: message exceeds 1 MiB")

// maxQueued bounds incoming requests and notifications waiting for the
// dispatcher. The reader never blocks on the dispatcher (responses must keep
// flowing), so a flooding peer gets rate_limited errors and dropped
// notifications instead of unbounded memory.
const maxQueued = 1024

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// dispatchFunc handles one incoming request (notify=false) or notification.
// The returned value is the result of a request; it is ignored for
// notifications.
type dispatchFunc func(ctx context.Context, method string, params json.RawMessage, notify bool) (any, error)

// conn is one side of an NDJSON JSON-RPC 2.0 connection. Responses are routed
// by the reader goroutine; incoming requests and notifications run one at a
// time, in order, on a separate dispatcher goroutine, so a handler may itself
// call the peer without deadlocking.
type conn struct {
	w      io.Writer
	closer io.Closer
	wmu    sync.Mutex

	next    atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan message
	closed  bool
	err     error
	done    chan struct{}

	queue    chan message
	dropped  atomic.Int64
	dispatch dispatchFunc
	ctx      context.Context
	cancel   context.CancelFunc
}

// newConn prepares a connection; start begins reading. Separating the two lets
// a dispatch closure capture the *conn before any message can arrive.
func newConn(w io.Writer, dispatch dispatchFunc) *conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &conn{w: w, pending: map[int64]chan message{}, done: make(chan struct{}), queue: make(chan message, maxQueued),
		dispatch: dispatch, ctx: ctx, cancel: cancel}
	if closer, ok := w.(io.Closer); ok {
		c.closer = closer
	}
	return c
}

func (c *conn) start(r io.Reader) {
	go c.readLoop(bufio.NewReaderSize(r, 64<<10))
	go c.dispatchLoop()
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > MaxMessageBytes+1 {
			return nil, errTooLarge
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
		return line, nil
	}
}

func (c *conn) readLoop(r *bufio.Reader) {
	for {
		line, err := readLine(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrClosed
			}
			c.shutdown(err)
			return
		}
		if len(line) <= 1 {
			continue
		}
		var m message
		if json.Unmarshal(line, &m) != nil || m.JSONRPC != "2.0" {
			c.shutdown(fmt.Errorf("extspi: malformed message"))
			return
		}
		if m.Method == "" {
			c.deliver(m)
			continue
		}
		select {
		case c.queue <- m:
		default:
			if len(m.ID) != 0 {
				c.reply(m.ID, nil, &Error{Code: CodeRateLimited, Message: "too many pending messages"})
			} else {
				c.dropped.Add(1)
			}
		}
	}
}

func (c *conn) deliver(m message) {
	id, err := strconv.ParseInt(string(m.ID), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

func (c *conn) dispatchLoop() {
	for {
		select {
		case <-c.done:
			return
		case m := <-c.queue:
			notify := len(m.ID) == 0
			result, err := c.safeDispatch(m, notify)
			if notify {
				continue
			}
			var rpcErr *Error
			if err != nil && !errors.As(err, &rpcErr) {
				rpcErr = &Error{Code: CodeInternalError, Message: truncate(err.Error(), 200)}
			}
			c.reply(m.ID, result, rpcErr)
		}
	}
}

func (c *conn) safeDispatch(m message, notify bool) (result any, err error) {
	defer func() {
		if p := recover(); p != nil {
			result, err = nil, &Error{Code: CodeInternalError, Message: "handler panicked"}
		}
	}()
	return c.dispatch(c.ctx, m.Method, m.Params, notify)
}

func (c *conn) reply(id json.RawMessage, result any, rpcErr *Error) {
	m := message{JSONRPC: "2.0", ID: id, Error: rpcErr}
	if rpcErr == nil {
		if result == nil {
			result = struct{}{}
		}
		raw, err := json.Marshal(result)
		if err != nil {
			m.Error = &Error{Code: CodeInternalError, Message: "result could not be encoded"}
		} else {
			m.Result = raw
		}
	}
	_ = c.write(m)
}

func (c *conn) write(m message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(raw) > MaxMessageBytes {
		return errTooLarge
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	_, err = c.w.Write(append(raw, '\n'))
	if err != nil {
		go c.shutdown(err)
	}
	return err
}

// call sends a request and decodes its result into out (which may be nil).
func (c *conn) call(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := c.next.Add(1)
	ch := make(chan message, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c.closeErr()
	}
	c.pending[id] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
	if err := c.write(message{JSONRPC: "2.0", ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: raw}); err != nil {
		forget()
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) != 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		forget()
		return ctx.Err()
	case <-c.done:
		return c.closeErr()
	}
}

func (c *conn) notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(message{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *conn) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed, c.err = true, err
	c.pending = map[int64]chan message{}
	close(c.done)
	c.mu.Unlock()
	c.cancel()
	if c.closer != nil {
		_ = c.closer.Close()
	}
}

func (c *conn) closeErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return ErrClosed
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
