package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/store"
)

// The composer's "/" suggestions come from the container's own Claude Code,
// so skills and project commands installed a moment ago show up too. A short
// cache keeps typing from starting a CLI on every keystroke.
const (
	slashCommandsTTL     = time.Minute
	slashCommandsTimeout = 20 * time.Second
)

type slashCommandsView struct {
	// available | stopped (no running container to ask) | unavailable
	// (Codex, or a CLI without the flags the listing needs).
	State    string               `json:"state"`
	Commands []agent.SlashCommand `json:"commands"`
}

func (s *Server) handleSlashCommands(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if _, err := s.sessionAccount(sess); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	view := slashCommandsView{State: "unavailable", Commands: []agent.SlashCommand{}}
	switch {
	case sess.Agent != config.AgentClaude:
	case sess.Status != store.StatusRunning || sess.ContainerID == "" || s.dock == nil:
		view.State = "stopped"
	default:
		commands, err := s.chat.room(sess.ID).slashCommands(r.Context(), sess)
		if err != nil {
			log.Printf("slash commands %s: %v", sess.ID, err)
			break
		}
		view.State, view.Commands = "available", commands
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, view)
}

// slashCommands asks the container's CLI for its commands without a prompt:
// no model request, nothing saved. One listing at a time per workspace; a
// failed refresh keeps the last list from the same container.
func (r *chatRoom) slashCommands(ctx context.Context, sess store.Session) ([]agent.SlashCommand, error) {
	r.commandsMu.Lock()
	defer r.commandsMu.Unlock()
	cached := r.commandsContainer == sess.ContainerID && r.commands != nil
	if cached && time.Since(r.commandsAt) < slashCommandsTTL {
		return r.commands, nil
	}
	commands, err := r.probeSlashCommands(ctx, sess)
	if err != nil {
		if cached {
			return r.commands, nil
		}
		return nil, err
	}
	r.commandsContainer, r.commandsAt, r.commands = sess.ContainerID, time.Now(), commands
	return commands, nil
}

func (r *chatRoom) probeSlashCommands(ctx context.Context, sess store.Session) ([]agent.SlashCommand, error) {
	help, err := r.cliHelp(ctx, sess)
	if err != nil {
		return nil, err
	}
	for _, flag := range agent.ClaudeCommandsFlags {
		if !strings.Contains(help, flag) {
			return nil, fmt.Errorf("claude CLI lacks %s", flag)
		}
	}
	// Account env and proxy as for a chat turn: the CLI may reach the network
	// while it starts, and must do so from the account's own exit.
	env, err := r.srv.execEnv(sess)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, slashCommandsTimeout)
	defer cancel()
	stream, err := r.srv.dock.ExecStream(ctx, sess.ContainerID, agent.ClaudeCommandsCommand(), env)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); err := stream.Demux(pw, io.Discard); pw.CloseWithError(err) }()
	defer func() { pr.Close(); stream.Close(); <-done }()
	commands, err := agent.ProbeClaudeCommands(stream, pr)
	_ = stream.CloseWrite() // stdin EOF ends the CLI
	return commands, err
}
