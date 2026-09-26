package server

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"agentbox/internal/store"
	"agentbox/internal/tunnel"
)

// The explicitly selected user tunnel is authoritative, including DNS. This
// path also supports compatible clients; the user's link enforces its allowlist
// again. Disabled/offline/denied connections never fall back to server egress.
func (s *Server) gitDial(ctx context.Context, c store.GitConnection, network, address string) (net.Conn, error) {
	if c.Network.Route != "tunnel" {
		return (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, address)
	}
	if gitActor(c) == "" || !s.cfg.GetTunnel().Enabled || s.tunnels == nil {
		return nil, errors.New("Git 用户隧道不可用")
	}
	session := s.tunnels.session(gitActor(c))
	if session == nil || session.IsClosed() {
		return nil, errors.New("Git 用户隧道未连接")
	}
	// OpenStream has its own timeout; closing one failed stream must not close
	// another user's or the whole user's shared tunnel session.
	stream, err := session.OpenStream()
	if err != nil {
		return nil, errors.New("Git 隧道连接失败")
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	_ = stream.SetDeadline(time.Now().Add(12 * time.Second))
	if err = tunnel.WriteConnect(stream, address); err != nil {
		stream.Close()
		return nil, errors.New("Git 隧道请求失败")
	}
	var status [1]byte
	if _, err = io.ReadFull(stream, status[:]); err != nil || status[0] != tunnel.StatusOK {
		stream.Close()
		return nil, errors.New("Git 隧道目标未放行或不可达")
	}
	if err = ctx.Err(); err != nil {
		stream.Close()
		return nil, err
	}
	_ = stream.SetDeadline(time.Time{})
	return stream, nil
}

func gitActor(c store.GitConnection) string {
	if c.Actor != "" {
		return c.Actor
	}
	return c.Owner
}
