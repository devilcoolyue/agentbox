package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

func clientTerminalSocket(id string) string { return "abox-client-" + id }

func clientTerminalCommand(sess store.Session, project store.ClientProject, terminal store.ClientTerminal, env []string) (string, error) {
	if !store.ValidClientResourceID(terminal.ID) || !store.ClientProjectPath(project.Path) {
		return "", store.ErrClientInvalid
	}
	var args []string
	switch terminal.Kind {
	case "shell":
		args = []string{"/bin/bash", "-l"}
	case "agent":
		if sess.Agent != config.AgentClaude && sess.Agent != config.AgentCodex {
			return "", store.ErrClientInvalid
		}
		args = append([]string{sess.Agent}, terminal.Arguments...)
	default:
		return "", store.ErrClientInvalid
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	tmux := "tmux -L " + clientTerminalSocket(terminal.ID)
	cmd := "command -v tmux >/dev/null || { printf 'Independent terminals require tmux. Update the workspace image.\\n'; exit 1; }\n"
	// Pin the socket directory regardless of user rc/environment. Each terminal
	// owns a tmux server so its environment cannot alter the legacy main session.
	cmd += "export TMUX_TMPDIR=/tmp\nunset TMUX\n"
	// Expand values from exec Env, not from the command line. This includes
	// admin-provided arbitrary environment keys without exposing their values.
	var sync strings.Builder
	have := map[string]bool{}
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && validEnvName(key) {
			have[key] = true
			fmt.Fprintf(&sync, "%s set-environment -g %s \"$%s\"; ", tmux, key, key)
		}
	}
	for _, key := range append(append([]string{}, tunnelEnvNames...), proxyEnvNames...) {
		if !have[key] {
			fmt.Fprintf(&sync, "%s set-environment -gu %s; ", tmux, key)
		}
	}
	cmd += "{ " + sync.String() + "} >/dev/null 2>&1\n"
	cmd += tmux + " has-session -t =work 2>/dev/null && exit 0\n"
	cmd += "exec " + tmux + " -u new-session -d -s work -c " + shellQuote(path.Join("/workspace", project.Path)) + " " + shellQuote("exec "+strings.Join(quoted, " "))
	return cmd, nil
}

func clientTerminalStopCommand(id string) string {
	// ID validation happens before this helper. A stale socket returns an error,
	// preserving the closing row for retry instead of falsely reporting success.
	socket := clientTerminalSocket(id)
	return fmt.Sprintf("export TMUX_TMPDIR=/tmp\nunset TMUX\nif [ ! -S \"/tmp/tmux-$(id -u)/%s\" ]; then exit 0; fi\nexec tmux -L %s kill-server", socket, socket)
}

func (s *Server) handleClientTerminalWS(w http.ResponseWriter, r *http.Request, sess store.Session) {
	id := r.PathValue("terminal")
	terminal, err := s.store.ClientTerminal(sess.ID, id)
	if err != nil {
		writeClientError(w, err)
		return
	}
	if terminal.State != "open" {
		writeClientError(w, store.ErrClientConflict)
		return
	}
	if !s.admitTerminal(w, r, sess) {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// ExecPTY retains this context for its lifetime. Cancel only if opening takes
	// too long; after opening, websocket and server lifetimes own cancellation.
	timer := time.AfterFunc(60*time.Second, cancel)
	defer timer.Stop()
	sess, err = s.startSession(ctx, sess)
	if err != nil {
		writeClientError(w, err)
		return
	}
	var pty *dockerx.PTY
	held := false
	defer func() {
		if held {
			s.workspaces().Activity().Release(sess.ID)
		}
	}()
	err = s.workspaces().UseRunning(ctx, sess.ID, func(current store.Session) error {
		terminal, err = s.store.ClientTerminal(current.ID, id)
		if err != nil {
			return err
		}
		if terminal.State != "open" {
			return store.ErrClientConflict
		}
		project, err := s.store.ClientProject(current.ID, terminal.ProjectID)
		if err != nil {
			return err
		}
		if err = s.checkClientProjectDir(current, project.Path); err != nil {
			return err
		}
		if why := s.quotaBlock(current.User); why != "" {
			return errors.New("terminal quota denied")
		}
		env, err := s.execEnv(current)
		if err != nil {
			return err
		}
		command, err := clientTerminalCommand(current, project, terminal, env)
		if err != nil {
			return err
		}
		// Wait for tmux creation while holding the workspace lock. A late PTY
		// attachment can never recreate a terminal after DELETE acknowledged it.
		_, err = s.dock.ExecCommandEnv(ctx, current.ContainerID, []string{"timeout", "-k", "2", "15", "/bin/bash", "-c", command}, env)
		if err != nil {
			return err
		}
		attach := "export TMUX_TMPDIR=/tmp\nunset TMUX\nexec tmux -L " + clientTerminalSocket(id) + " -u attach-session -d -t =work"
		pty, err = s.dock.ExecPTY(ctx, current.ContainerID, []string{"/bin/bash", "-c", attach}, env)
		if err == nil {
			s.workspaces().Activity().Hold(current.ID)
			held = true
			sess = current
		}
		return err
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	timer.Stop()
	s.bridgeTermPTY(w, r, sess, pty, func() error {
		current, err := s.store.ClientTerminal(sess.ID, id)
		if err != nil {
			return err
		}
		if current.State != "open" {
			return store.ErrClientConflict
		}
		return nil
	})
}
