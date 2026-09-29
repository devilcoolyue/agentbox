package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"agentbox/internal/dockerx"
	"agentbox/internal/store"
	"github.com/gorilla/websocket"
)

func validBrowserURL(raw string) bool {
	if raw == "" {
		return true
	}
	if len(raw) > 8192 || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil
}

// NetworkKey detects changes to the account's proxy binding. The browser is
// restarted on next start; an old attached desktop is disconnected promptly.
func (s *Server) browserEnv(sess store.Session) ([]string, string, error) {
	if _, err := s.sessionAccount(sess); err != nil {
		return nil, "", err
	}
	env := []string{}
	p, bound := s.cfg.AccountProxy(sess.AccountID)
	if bound {
		if p.Disabled {
			return nil, "", errors.New("账号出口代理已停用")
		}
		var proxy string
		for _, kv := range s.proxyEnvList(sess) {
			if strings.HasPrefix(kv, "HTTP_PROXY=") {
				proxy = strings.TrimPrefix(kv, "HTTP_PROXY=")
			}
		}
		if proxy == "" || !s.bridgeUp.Load() {
			return nil, "", errors.New("账号出口代理尚未就绪")
		}
		env = append(env, "AGENTBOX_BROWSER_PROXY="+proxy)
	}
	raw, _ := json.Marshal(struct {
		Proxy any
		Bound bool
		Env   []string
	}{p, bound, env})
	digest := sha256.Sum256(raw)
	key := hex.EncodeToString(digest[:])
	env = append(env, "AGENTBOX_BROWSER_NETWORK_KEY="+key)
	return env, key, nil
}

func (s *Server) handleBrowser(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if _, err := s.sessionAccount(sess); err != nil {
		writeErr(w, 403, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	action, target := "status", ""
	if r.Method == http.MethodPost {
		var req struct {
			URL string `json:"url"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF || !validBrowserURL(req.URL) {
			writeErr(w, 400, "请输入有效的 HTTP / HTTPS 地址")
			return
		}
		if why := s.quotaBlock(sess.User); why != "" {
			writeErr(w, 403, why)
			return
		}
		target, action = req.URL, "start"
		var err error
		sess, err = s.startSession(ctx, sess)
		if err != nil {
			writeErr(w, 409, err.Error())
			return
		}
	} else if r.Method == http.MethodDelete {
		action = "stop"
	}
	running, err := s.dock.Running(ctx, sess.ContainerID)
	if err != nil {
		writeErr(w, 503, "无法读取容器状态")
		return
	}
	if !running {
		writeJSON(w, 200, dockerx.BrowserInfo{})
		return
	}
	var result dockerx.BrowserInfo
	err = s.workspaces().UseRunning(ctx, sess.ID, func(cur store.Session) error {
		var env []string
		if action == "start" {
			var err error
			env, _, err = s.browserEnv(cur)
			if err != nil {
				return err
			}
		}
		var err error
		result, err = s.dock.BrowserCommand(ctx, cur.ContainerID, action, target, env)
		return err
	})
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	if action == "start" && !result.Available {
		writeErr(w, 409, "当前镜像未安装浏览器，请管理员构建并选择 agentbox-agent:browser 镜像，再停止并启动空间")
		return
	}
	writeJSON(w, 200, result)
}

// A WebSocket transports raw RFB to a loopback-only VNC server using Docker
// exec. There is no host port, reverse proxy target, or externally reachable CDP.
func (s *Server) handleBrowserWS(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if why := s.quotaBlock(sess.User); why != "" {
		writeErr(w, 403, why)
		return
	}
	_, networkKey, err := s.browserEnv(sess)
	if err != nil {
		writeErr(w, 403, err.Error())
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var stream *dockerx.Stream
	err = s.workspaces().UseRunning(ctx, sess.ID, func(cur store.Session) error {
		checkCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		defer stop()
		state, err := s.dock.BrowserCommand(checkCtx, cur.ContainerID, "status", "", nil)
		if err != nil {
			return err
		}
		if !state.Running || state.NetworkKey != networkKey {
			return errors.New("请重新启动浏览器以应用当前网络设置")
		}
		stream, err = s.dock.ExecStream(ctx, cur.ContainerID, []string{"python3", "-I", "/opt/agentbox/browser.py", "relay"}, nil)
		return err
	})
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	defer stream.Close()
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	release, ok := s.track(func() { conn.Close(); stream.Close() })
	if !ok {
		return
	}
	defer release()
	s.workspaces().Activity().Hold(sess.ID)
	defer s.workspaces().Activity().Release(sess.ID)
	conn.SetReadLimit(4 << 20)
	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(wsPongWait)) })
	done, pingDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer conn.Close()
		_ = stream.Demux(browserWSWriter{conn}, io.Discard)
	}()
	go func() {
		defer close(pingDone)
		tick := time.NewTicker(wsPingPeriod)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				// Recheck the actual login token, workspace ownership and account grant.
				user, ok := s.store.TokenUser(bearerToken(r))
				cur, exists := s.store.Get(sess.ID)
				_, key, err := s.browserEnv(cur)
				if !ok || !exists || user.Name != cur.User || err != nil || key != networkKey || s.quotaBlock(cur.User) != "" {
					closeWithReason(conn, closeAccountAccess, "授权或网络配置已变更，请重新连接浏览器")
					conn.Close()
					return
				}
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)) != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	defer func() { cancel(); conn.Close(); stream.Close(); <-done; <-pingDone }()
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage {
			return
		}
		if _, key, err := s.browserEnv(sess); err != nil || key != networkKey {
			return
		}
		if _, err := stream.Write(data); err != nil {
			return
		}
	}
}

type browserWSWriter struct{ conn *websocket.Conn }

func (w browserWSWriter) Write(p []byte) (int, error) {
	w.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	if err := w.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *Server) handleBrowserClipboard(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if why := s.quotaBlock(sess.User); why != "" {
		writeErr(w, 403, why)
		return
	}
	_, key, err := s.browserEnv(sess)
	if err != nil {
		writeErr(w, 403, err.Error())
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if r.Method == http.MethodPost {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 300000))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF || len(req.Text) > 48<<10 {
			writeErr(w, 400, "剪贴板最多支持 48 KiB 的文字")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var text string
	err = s.workspaces().UseRunning(ctx, sess.ID, func(cur store.Session) error {
		state, err := s.dock.BrowserCommand(ctx, cur.ContainerID, "status", "", nil)
		if err != nil {
			return err
		}
		if !state.Running || state.NetworkKey != key {
			return errors.New("请先连接浏览器")
		}
		text, err = s.dock.BrowserClipboard(ctx, cur.ContainerID, r.Method == http.MethodPost, req.Text)
		return err
	})
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]string{"text": text})
}
