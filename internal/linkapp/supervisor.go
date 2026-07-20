package linkapp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"agentbox/internal/tunnel"
)

// State is what the panel's status light shows.
type State string

const (
	StateStopped    State = "stopped"
	StateConnecting State = "connecting"
	StateOnline     State = "online"
	// StateAuthFailed is terminal until the user re-pairs: the server rejected
	// the stored token, and the panel holds no password to get a new one.
	StateAuthFailed State = "auth_failed"
)

// Status is the supervisor snapshot the panel renders.
type Status struct {
	State  State       `json:"state"`
	Detail string      `json:"detail"`
	Since  int64       `json:"since"` // unix millis the current session came online, 0 if not online
	Maps   []MapStatus `json:"maps"`
}

// Supervisor owns the tunnel's lifecycle: one connection attempt loop that can
// be started and stopped from the panel, with its state and log observable
// while it runs.
type Supervisor struct {
	// Relogin, when set, is called after the server rejects the stored token
	// and returns a fresh one. The headless CLI sets it (it holds a password);
	// the panel leaves it nil, because pairing deliberately keeps no password —
	// there the user re-pairs instead.
	Relogin func() (string, error)

	log *logRing

	mu      sync.Mutex
	state   State
	detail  string
	since   time.Time
	maps    []MapStatus
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewSupervisor returns an idle supervisor.
func NewSupervisor() *Supervisor {
	return &Supervisor{log: newLogRing(500), state: StateStopped}
}

// Logf writes a line into the panel's log view.
func (s *Supervisor) Logf(format string, args ...any) { s.log.Printf(format, args...) }

// Logs returns log lines newer than seq, plus the newest sequence number.
func (s *Supervisor) Logs(seq int64) ([]LogLine, int64) { return s.log.Since(seq) }

// MirrorTo echoes every log line to fn as well as into the ring buffer.
func (s *Supervisor) MirrorTo(fn func(string)) {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	s.log.mirror = fn
}

// Status returns the current snapshot.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{State: s.state, Detail: s.detail, Maps: s.maps}
	if s.state == StateOnline && !s.since.IsZero() {
		st.Since = s.since.UnixMilli()
	}
	return st
}

// Running reports whether a connection loop is active.
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Start validates cfg and launches the connection loop. It returns an error
// the panel can show directly; the loop itself never fails fatally, it retries.
func (s *Supervisor) Start(cfg Config) error {
	if !cfg.Paired() {
		return errors.New("尚未接入服务器，请先粘贴配对码")
	}
	wl, maps, err := cfg.Validate()
	if err != nil {
		return err
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("隧道已经在运行")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.running, s.cancel, s.done = true, cancel, done
	s.state, s.detail, s.maps = StateConnecting, "正在连接…", nil
	s.mu.Unlock()

	s.log.Printf("启动隧道：%s（用户 %s，%d 条放行规则，%d 个端口映射）",
		cfg.Server, cfg.User, len(wl), len(maps))
	go s.run(ctx, cfg, wl, maps, done)
	return nil
}

// Stop cancels the loop and waits for it to wind down.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	s.log.Printf("正在停止隧道…")
	cancel()
	if done != nil {
		<-done
	}
}

func (s *Supervisor) run(ctx context.Context, cfg Config, wl tunnel.Whitelist, maps []tunnel.MapSpec, done chan struct{}) {
	defer close(done)
	defer func() {
		s.mu.Lock()
		s.running, s.cancel, s.done = false, nil, nil
		s.since, s.maps = time.Time{}, nil
		// An auth failure is the reason we stopped — keep it on screen instead
		// of overwriting it with a bland "stopped".
		if s.state != StateAuthFailed {
			s.state, s.detail = StateStopped, ""
		}
		s.mu.Unlock()
		s.log.Printf("隧道已停止")
	}()

	link := &tunnel.Link{Whitelist: wl, Logf: s.log.Printf}
	backoff := time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		s.setState(StateConnecting, "正在连接 "+cfg.Server+" …")

		start := time.Now()
		err := s.serveOnce(ctx, link, cfg, maps)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Printf("隧道断开：%v", err)
		}

		if errors.Is(err, ErrAuthRejected) {
			// The token is dead; retrying it is pointless. Get a new one if we
			// can, otherwise stop and say what the user has to do.
			if s.Relogin == nil {
				s.setState(StateAuthFailed, "登录凭证已失效（多半是改过密码），请在 agentbox 里重新生成配对码")
				s.log.Printf("令牌已被服务器拒绝，停止重连——请重新配对")
				return
			}
			newToken, lerr := s.Relogin()
			if lerr != nil {
				s.log.Printf("重新登录失败：%v", lerr)
			} else {
				cfg.Token = newToken
				backoff = time.Second
				s.log.Printf("已重新登录为 %s", cfg.User)
				continue
			}
		}

		// A connection that stayed up a while earned a fresh backoff budget.
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		s.setState(StateConnecting, fmt.Sprintf("连接中断，%s 后重试…", backoff))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// serveOnce holds one tunnel connection open until it drops or ctx is done.
func (s *Supervisor) serveOnce(ctx context.Context, link *tunnel.Link, cfg Config, maps []tunnel.MapSpec) error {
	sess, mapStatus, err := dialTunnel(ctx, cfg, maps)
	if err != nil {
		return err
	}
	defer sess.Close()

	for _, m := range mapStatus {
		if m.OK {
			s.log.Printf("端口映射 %s：服务端已生效", m.Port)
		} else {
			s.log.Printf("端口映射 %s：服务端绑定失败：%s", m.Port, m.Detail)
		}
	}

	s.mu.Lock()
	s.state, s.detail, s.since, s.maps = StateOnline, "", time.Now(), mapStatus
	s.mu.Unlock()
	s.log.Printf("隧道已建立，容器现在可以访问你的内网了")

	// Stopping the panel must unblock Serve, which is parked in AcceptStream.
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	return link.Serve(sess)
}

func (s *Supervisor) setState(st State, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state, s.detail = st, detail
	if st != StateOnline {
		s.since, s.maps = time.Time{}, nil
	}
}
