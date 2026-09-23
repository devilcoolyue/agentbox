package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// runtimeState owns work and connections whose lifetime exceeds an HTTP
// request. Admission and WaitGroup.Add share a lock with shutdown so no work
// can race into an already-drained server.
type runtimeState struct {
	mu        sync.Mutex
	stopping  bool
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	resources map[*resource]struct{}
	closeOnce sync.Once
	done      chan struct{}
	err       error
}

type resource struct{ close func() }

func (s *Server) runtime() *runtimeState {
	s.runtimeOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.life = &runtimeState{ctx: ctx, cancel: cancel, resources: map[*resource]struct{}{}, done: make(chan struct{})}
	})
	return s.life
}

func (s *Server) workContext() context.Context { return s.runtime().ctx }

func (l *runtimeState) begin() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopping {
		return false
	}
	l.wg.Add(1)
	return true
}

func (s *Server) spawn(fn func()) bool {
	l := s.runtime()
	if !l.begin() {
		return false
	}
	go func() { defer l.wg.Done(); fn() }()
	return true
}

func (s *Server) admit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := s.runtime()
		if !l.begin() {
			writeErr(w, http.StatusServiceUnavailable, "server shutting down")
			return
		}
		defer l.wg.Done()
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(l.ctx, cancel)
		defer stop()
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// track also handles a resource that finishes opening during shutdown: it is
// immediately closed instead of escaping the shutdown snapshot.
func (s *Server) track(close func()) (release func(), ok bool) {
	l := s.runtime()
	r := &resource{close: close}
	l.mu.Lock()
	if l.stopping {
		l.mu.Unlock()
		close()
		return func() {}, false
	}
	l.resources[r] = struct{}{}
	l.mu.Unlock()
	return func() { l.mu.Lock(); delete(l.resources, r); l.mu.Unlock() }, true
}

// Close is idempotent. A deadline bounds the caller's wait, not cleanup itself:
// dependencies stay open until all admitted users have finished with them.
func (s *Server) Close(ctx context.Context) error {
	l := s.runtime()
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.stopping = true
		l.mu.Unlock()
		go func() {
			// Stop provider work before detaching its output. Existing terminal
			// tmux jobs and session containers intentionally survive a restart.
			if s.chat != nil {
				interruptCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				s.chat.interruptAll(interruptCtx)
				cancel()
			}
			l.cancel()
			l.mu.Lock()
			resources := l.resources
			l.resources = map[*resource]struct{}{}
			l.mu.Unlock()
			for r := range resources {
				r.close()
			}
			if s.tunnels != nil {
				s.tunnels.closeAll()
			}
			l.wg.Wait()
			if s.dock != nil {
				l.err = errors.Join(l.err, s.dock.Close())
			}
			if s.store != nil {
				l.err = errors.Join(l.err, s.store.Close())
			}
			close(l.done)
		}()
	})
	select {
	case <-l.done:
		return l.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitInterval(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// serveConnections owns both the listener and every accepted connection;
// protocol implementations can spawn internal copying goroutines, but their
// outer handler must finish before server dependencies are released.
func (s *Server) serveConnections(ln net.Listener, handle func(net.Conn)) {
	untrack, ok := s.track(func() { _ = ln.Close() })
	if !ok {
		return
	}
	defer untrack()
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		release, ok := s.track(func() { _ = conn.Close() })
		if !ok {
			return
		}
		if !s.spawn(func() { defer release(); defer conn.Close(); handle(conn) }) {
			release()
			_ = conn.Close()
			return
		}
	}
}
