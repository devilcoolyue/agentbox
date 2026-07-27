package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"agentbox/internal/store"
)

const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 90 * time.Second
	wsPingPeriod = 30 * time.Second
)

// termCommand builds the terminal's entry command: mirror this exec's env into
// the tmux server's global environment, then attach.
//
// The exec env reaches this process only. A tmux session that already exists
// was forked from some earlier exec, so anything started in it keeps that older
// env — a tunnel that came up after the session started stays invisible there
// (the symptom: the agent reports no $AGENTBOX_INTRANET_PROXY while the link is
// plainly connected). Pushing the values into tmux's global environment makes
// every window/pane opened from now on, including a restarted agent, see the
// current values. Panes already running keep their frozen env; that is a
// process-level fact of life, not something tmux can undo.
func termCommand(env []string) string {
	cmd := "command -v tmux >/dev/null || exec /bin/bash\n"
	// Errors are dropped on purpose: with no tmux server yet these fail, which
	// is harmless — new-session then inherits this exec's env directly. Their
	// output must not reach the PTY either way.
	if sync := tmuxEnvSync(env); sync != "" {
		cmd += "{ " + sync + "} >/dev/null 2>&1\n"
	}
	return cmd + "exec tmux -u new-session -A -D -s main"
}

// tmuxEnvSync renders the `tmux set-environment` calls mirroring env into the
// tmux global environment. Tunnel variables missing from env are unset rather
// than left alone, so a dropped link stops advertising a dead proxy URL to
// newly opened shells.
func tmuxEnvSync(env []string) string {
	var b strings.Builder
	have := make(map[string]bool, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !validEnvName(k) {
			continue
		}
		have[k] = true
		fmt.Fprintf(&b, "tmux set-environment -g %s %s; ", k, shellQuote(v))
	}
	for _, k := range tunnelEnvNames {
		if !have[k] {
			fmt.Fprintf(&b, "tmux set-environment -gu %s; ", k)
		}
	}
	return b.String()
}

// validEnvName keeps anything that would not survive interpolation out of the
// generated script; account env is admin-edited config, not assumed well-formed.
func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// shellQuote single-quotes s for the bash -c script (embedded quotes are broken
// out and escaped), so a value can never end the quoting and inject commands.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// handleTermWS bridges a browser xterm.js to a TTY exec inside the session
// container. Protocol: binary frames are raw terminal bytes in both
// directions; text frames are JSON control messages ({"type":"resize",...}).
func (s *Server) handleTermWS(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	sess, err := s.startSession(ctx, sess)
	cancel()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "start session: "+err.Error())
		return
	}

	// Attach to a persistent tmux session instead of spawning a bare bash:
	// docker exec detach does NOT kill the process, so a bare shell (and any
	// claude/codex inside) would linger unreachable after a reconnect. With
	// tmux, reconnects land back in the same session, programs survive.
	//   -A  attach if the session exists, create otherwise
	//   -D  detach other clients (last connection wins; a displaced client's
	//       exec exits cleanly, and the frontend treats clean closes as final
	//       rather than auto-reconnecting, so two tabs don't fight)
	// Containers built from pre-tmux images fall back to a plain bash.
	env := s.execEnv(sess)
	pty, err := s.dock.ExecPTY(context.Background(), sess.ContainerID, []string{"/bin/bash", "-c", termCommand(env)}, env)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "exec: "+err.Error())
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		pty.Close()
		return
	}
	defer conn.Close()
	defer pty.Close()

	// An attached terminal runs a live PTY exec inside the container; hold it
	// so the idle reaper can't stop the container out from under the shell.
	s.idle.hold(sess.ID)
	defer s.idle.release(sess.ID)

	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})

	done := make(chan struct{})

	// container -> browser
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			n, err := pty.Reader.Read(buf)
			if n > 0 {
				conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "process exited"),
					time.Now().Add(wsWriteWait))
				return
			}
		}
	}()

	// keepalive pings
	go func() {
		t := time.NewTicker(wsPingPeriod)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait))
			}
		}
	}()

	// browser -> container
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		switch mt {
		case websocket.BinaryMessage:
			if _, err := pty.Conn.Write(data); err != nil {
				return
			}
		case websocket.TextMessage:
			var ctl struct {
				Type string `json:"type"`
				Cols uint   `json:"cols"`
				Rows uint   `json:"rows"`
			}
			if err := json.Unmarshal(data, &ctl); err == nil && ctl.Type == "resize" && ctl.Cols > 0 && ctl.Rows > 0 {
				if err := s.dock.ResizePTY(r.Context(), pty.ExecID, ctl.Cols, ctl.Rows); err != nil {
					log.Printf("resize %s: %v", sess.ID, err)
				}
			}
		}
	}
}
