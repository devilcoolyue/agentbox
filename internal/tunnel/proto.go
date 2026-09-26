// Package tunnel holds the wire protocol shared by the agentbox server (which
// exposes a SOCKS5 proxy backed by a reverse tunnel) and the abox-link client
// (which runs on the user's machine and dials out to their LAN/intranet).
//
// Transport: a single WebSocket carries a yamux session. The server is the
// yamux client (it opens a stream per proxied connection); abox-link is the
// yamux server (it accepts streams and dials the real target). Each stream
// begins with a small request header, the link answers with a status byte, and
// the rest is a raw bidirectional byte pipe.
package tunnel

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// Stream request types. A type byte prefixes every stream so the wire format
// can grow (e.g. a future fixed-port forward) without breaking older peers.
const (
	StreamConnect byte = 1 // followed by a target "host:port"; SOCKS CONNECT
)

// Status bytes the link returns before the byte stream begins.
const (
	StatusOK        byte = 0 // dialed, piping follows
	StatusDenied    byte = 1 // target rejected by the local whitelist
	StatusDialError byte = 2 // target allowed but dial failed
)

// WriteConnect writes a CONNECT request header (type + length-prefixed addr).
func WriteConnect(w io.Writer, addr string) error {
	if len(addr) == 0 || len(addr) > 0xffff {
		return fmt.Errorf("tunnel: addr length %d out of range", len(addr))
	}
	var hdr [3]byte
	hdr[0] = StreamConnect
	binary.BigEndian.PutUint16(hdr[1:], uint16(len(addr)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := io.WriteString(w, addr)
	return err
}

// ReadRequest reads the stream header, returning the type and (for CONNECT) the
// target address.
func ReadRequest(r io.Reader) (typ byte, addr string, err error) {
	var t [1]byte
	if _, err = io.ReadFull(r, t[:]); err != nil {
		return 0, "", err
	}
	switch t[0] {
	case StreamConnect:
		var l [2]byte
		if _, err = io.ReadFull(r, l[:]); err != nil {
			return 0, "", err
		}
		buf := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err = io.ReadFull(r, buf); err != nil {
			return 0, "", err
		}
		return t[0], string(buf), nil
	default:
		return 0, "", fmt.Errorf("tunnel: unknown stream type %d", t[0])
	}
}

// WSConn adapts a gorilla WebSocket to net.Conn so yamux can run over it. It
// serializes writes (WebSocket forbids concurrent writers) and reassembles
// binary frames into a byte stream.
type WSConn struct {
	c   *websocket.Conn
	r   io.Reader
	wmu sync.Mutex
	rmu sync.Mutex
}

func NewWSConn(c *websocket.Conn) *WSConn { return &WSConn{c: c} }

func (w *WSConn) Read(p []byte) (int, error) {
	w.rmu.Lock()
	defer w.rmu.Unlock()
	for {
		if w.r == nil {
			mt, r, err := w.c.NextReader()
			if err != nil {
				return 0, err
			}
			if mt != websocket.BinaryMessage {
				continue // ignore pings/text; keepalive is control frames
			}
			w.r = r
		}
		n, err := w.r.Read(p)
		if err == io.EOF {
			w.r = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (w *WSConn) Write(p []byte) (int, error) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if err := w.c.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *WSConn) Close() error         { return w.c.Close() }
func (w *WSConn) LocalAddr() net.Addr  { return w.c.LocalAddr() }
func (w *WSConn) RemoteAddr() net.Addr { return w.c.RemoteAddr() }

func (w *WSConn) SetDeadline(t time.Time) error {
	if err := w.c.SetReadDeadline(t); err != nil {
		return err
	}
	return w.c.SetWriteDeadline(t)
}
func (w *WSConn) SetReadDeadline(t time.Time) error  { return w.c.SetReadDeadline(t) }
func (w *WSConn) SetWriteDeadline(t time.Time) error { return w.c.SetWriteDeadline(t) }

// Pipe preserves TCP and yamux half-closes so request protocols can signal
// end-of-input before receiving a response. Errors abort both directions.
func Pipe(a, b io.ReadWriteCloser) {
	var once sync.Once
	closeBoth := func() { once.Do(func() { a.Close(); b.Close() }) }
	defer closeBoth()
	halfClose := func(c io.ReadWriteCloser) {
		switch v := c.(type) {
		case interface{ CloseWrite() error }:
			_ = v.CloseWrite()
		case *yamux.Stream:
			_ = v.Close() // yamux Close sends FIN; reads remain usable.
		default:
			closeBoth()
		}
	}
	done := make(chan struct{})
	go func() {
		if _, err := io.Copy(b, a); err != nil {
			closeBoth()
		} else {
			halfClose(b)
		}
		close(done)
	}()
	if _, err := io.Copy(a, b); err != nil {
		closeBoth()
	} else {
		halfClose(a)
	}
	<-done
}
