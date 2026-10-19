package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/store"
)

const (
	gatewayLiveMarker = "AGENTBOX_GATEWAY_OK"
	gatewayLiveLoop   = "AGENTBOX_GATEWAY_LOOP"
)

// gatewayLiveCall is one request the synthetic provider received.
type gatewayLiveCall struct {
	Path, Auth string
	Loop       bool
	Stream     bool
	Sent       int  // loop deltas written before the stream ended
	Cancelled  bool // the gateway cut the upstream request
}

type gatewayLiveUpstream struct {
	mu    sync.Mutex
	calls []gatewayLiveCall
}

func (u *gatewayLiveUpstream) snapshot() []gatewayLiveCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]gatewayLiveCall(nil), u.calls...)
}

// ServeHTTP answers Anthropic Messages and OpenAI Responses streams. A prompt
// carrying gatewayLiveLoop gets an endless short-sentence repeat, the shape the
// loop guard interrupts; anything else gets one marker reply.
func (u *gatewayLiveUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	auth := r.Header.Get("Authorization")
	if auth == "" {
		auth = "x-api-key " + r.Header.Get("X-Api-Key")
	}
	call := gatewayLiveCall{Path: r.URL.Path, Auth: auth, Loop: bytes.Contains(raw, []byte(gatewayLiveLoop))}
	defer func() {
		call.Cancelled = r.Context().Err() != nil
		u.mu.Lock()
		u.calls = append(u.calls, call)
		u.mu.Unlock()
	}()
	messages, responses := strings.HasSuffix(r.URL.Path, "/v1/messages"), strings.HasSuffix(r.URL.Path, "/v1/responses")
	if r.Method != http.MethodPost || !messages && !responses {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(raw, &body)
	call.Stream = body.Stream
	if messages && !body.Stream {
		// A non-streaming reply is never inspected: a loop here runs to the
		// token limit. Only a cooldown at the gateway keeps it from starting.
		reply := gatewayLiveMarker
		if call.Loop {
			reply = strings.Repeat("Go.\n", 4000)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "msg_live", "type": "message", "role": "assistant", "model": body.Model, "stop_reason": "max_tokens",
			"content": []any{map[string]any{"type": "text", "text": reply}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 16000}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flush := w.(http.Flusher).Flush
	event := func(name string, v any) {
		b, _ := json.Marshal(v)
		io.WriteString(w, "event: "+name+"\ndata: "+string(b)+"\n\n")
		flush()
	}
	text := func(delta string) {
		if messages {
			event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": delta}})
		} else {
			event("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_live", "output_index": 0, "content_index": 0, "delta": delta})
		}
	}
	if messages {
		event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_live", "type": "message", "role": "assistant", "model": body.Model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 0}}})
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	} else {
		event("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_live", "object": "response", "status": "in_progress", "output": []any{}}})
		event("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{"id": "msg_live", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
		event("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "msg_live", "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	}
	if call.Loop {
		for ; call.Sent < 4000 && r.Context().Err() == nil; call.Sent++ {
			text("Go.\n")
			time.Sleep(time.Millisecond)
		}
		return // never completes: only an interruption ends it early
	}
	text(gatewayLiveMarker)
	if messages {
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 2}})
		event("message_stop", map[string]any{"type": "message_stop"})
		return
	}
	item := map[string]any{"id": "msg_live", "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": gatewayLiveMarker, "annotations": []any{}}}}
	event("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": "msg_live", "output_index": 0, "content_index": 0, "text": gatewayLiveMarker})
	event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	event("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_live", "object": "response", "status": "completed",
		"output": []any{item}, "usage": map[string]any{"input_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens": 2,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": 12}}})
}

// gatewayLiveEnv is a clean CLI environment: nothing from this machine's own
// login, proxy or config leaks in; the session's exec env is added on top.
func gatewayLiveEnv(t *testing.T, cwd string, extra []string) []string {
	t.Helper()
	home := t.TempDir()
	onboarded, _ := json.Marshal(map[string]any{"hasCompletedOnboarding": true, "bypassPermissionsModeAccepted": true,
		"projects": map[string]any{cwd: map[string]any{"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true}}})
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), onboarded, 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C.UTF-8", "TERM=dumb",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "ENABLE_TOOL_SEARCH=false"}
	return append(env, extra...)
}

// gatewayLiveRun runs a product chat argv without its PID wrapper: the wrapper
// writes the fixed in-container PID file, which on a workspace machine belongs
// to the real web chat's interrupt path.
func gatewayLiveRun(t *testing.T, wrapped []string, cwd string, env []string, prompt string) ([]map[string]any, error) {
	t.Helper()
	if len(wrapped) < 5 || wrapped[0] != "/bin/sh" {
		t.Fatalf("unexpected chat argv shape: %q", wrapped)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, wrapped[4], wrapped[5:]...)
	cmd.Dir, cmd.Env, cmd.Stdin = cwd, env, strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var events []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			events = append(events, ev)
		}
	}
	if err != nil {
		t.Logf("%s exited: %v; stderr tail: %s", wrapped[4], err, liveTail(stderr.String(), 600))
	}
	return events, err
}

func liveTail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func gatewayLiveText(events []map[string]any) string {
	var b strings.Builder
	for _, ev := range events {
		raw, _ := json.Marshal(ev)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

func waitExtensionRunning(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.extensions.Status("agentbox.loop-guard").State != "running" {
		if time.Now().After(deadline) {
			t.Fatal("extension did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The installed Claude Code and Codex CLIs reach a synthetic provider through
// the model gateway exactly as a workspace does: Claude from execEnv's
// ANTHROPIC_BASE_URL with its /x/<session>/<mac> prefix, Codex (exec and
// app-server) from the session config.toml the binding rewrote. A normal reply
// passes, a repeating stream is interrupted, and the CLI's identical retry is
// refused without reaching the provider. No container, account or paid model.
func TestModelGatewayCLILive(t *testing.T) {
	if os.Getenv("AGENTBOX_GATEWAY_CLI_TEST") == "" {
		t.Skip("set AGENTBOX_GATEWAY_CLI_TEST=1 to run the installed claude/codex through the model gateway (synthetic provider, no model calls)")
	}
	upstream := &gatewayLiveUpstream{}
	provider := httptest.NewServer(upstream)
	defer provider.Close()
	s, sess := extensionTestServer(t, provider.URL+"/api")
	acct, _ := s.cfg.Account("codex")
	pool := writeCodexRelayPool(t, acct.CredentialsDir, provider.URL+"/v1")
	codexSess := store.Session{ID: "live-codex", User: "alice", Name: "codex", Agent: config.AgentCodex, AccountID: "codex"}
	if err := s.store.Put(codexSess); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(s.homeDir(codexSess), ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionConfig := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(sessionConfig, append([]byte("model = \"gpt-5.5\"\n"), pool...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"OPENAI_API_KEY":"codex-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	enableLoopGuard(t, s, 0, "relay", "codex")
	waitExtensionRunning(t, s)
	if v := s.modelGatewayView(); !v.Active {
		t.Fatalf("gateway not listening: %+v", v)
	}
	waitForFile(t, sessionConfig, s.modelGatewayURL(codexSess.ID))

	// check runs one prompt and returns the provider calls and loop guard
	// events it caused.
	check := func(t *testing.T, name string, run func(prompt string) (string, error), prompt string) ([]gatewayLiveCall, int, string, error) {
		before, events := len(upstream.snapshot()), len(s.extensions.Events("agentbox.loop-guard"))
		out, err := run(prompt)
		calls := upstream.snapshot()[before:]
		added := len(s.extensions.Events("agentbox.loop-guard")) - events
		t.Logf("%s: provider calls %+v, loop guard events +%d, exit %v", name, calls, added, err)
		return calls, added, out, err
	}
	expectReply := func(t *testing.T, name string, calls []gatewayLiveCall, added int, out string, err error, path, auth string) {
		t.Helper()
		if err != nil || !strings.Contains(out, gatewayLiveMarker) {
			t.Fatalf("%s: no marker reply (err %v):\n%s", name, err, liveTail(out, 2000))
		}
		if len(calls) == 0 || added != 0 {
			t.Fatalf("%s: calls %+v events +%d", name, calls, added)
		}
		for _, c := range calls {
			if c.Path != path || c.Auth != auth {
				t.Fatalf("%s: provider saw %s with %q, want %s with %q", name, c.Path, c.Auth, path, auth)
			}
		}
	}
	expectInterrupted := func(t *testing.T, name string, calls []gatewayLiveCall, added int, out string, fields ...string) {
		t.Helper()
		var loops int
		for _, c := range calls {
			if c.Loop {
				loops++
				if !c.Cancelled || c.Sent >= 4000 {
					t.Fatalf("%s: looping stream not cut: %+v", name, c)
				}
			}
		}
		// One interrupted stream; a CLI retry of the identical request is
		// refused by the cooldown at the gateway and never reaches the provider.
		if loops != 1 || added < 1 {
			t.Fatalf("%s: loop calls %d, events +%d", name, loops, added)
		}
		for _, f := range fields {
			if !strings.Contains(out, f) {
				t.Fatalf("%s: CLI output lacks %q:\n%s", name, f, liveTail(out, 2000))
			}
		}
		if strings.Contains(out, gatewayLiveMarker) {
			t.Fatalf("%s: looping turn reported the marker", name)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		last := lines[len(lines)-1]
		for _, l := range lines {
			var ev struct{ Type, Result string }
			if json.Unmarshal([]byte(l), &ev) == nil && ev.Type == "result" {
				last = ev.Result // Claude's turn result, as the web chat shows it
			}
		}
		t.Logf("%s: CLI ends with %s", name, liveTail(last, 400))
	}

	t.Run("claude", func(t *testing.T) {
		if _, err := exec.LookPath("claude"); err != nil {
			t.Fatal("claude not on PATH: the gated check cannot pass without it")
		}
		env, err := s.execEnv(sess)
		if err != nil {
			t.Fatal(err)
		}
		if got := envValue(env, envAnthropicBaseURL); got != s.modelGatewayURL(sess.ID) {
			t.Fatalf("ANTHROPIC_BASE_URL %q", got)
		}
		argv, err := agent.ChatCommand(config.AgentClaude, "bypassPermissions", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		cwd := t.TempDir()
		run := func(prompt string) (string, error) {
			events, err := gatewayLiveRun(t, argv, cwd, gatewayLiveEnv(t, cwd, env), prompt)
			return gatewayLiveText(events), err
		}
		calls, added, out, err := check(t, "claude reply", run, "Reply with the fixture marker.")
		expectReply(t, "claude reply", calls, added, out, err, "/api/v1/messages", "Bearer relay-token")
		calls, added, out, _ = check(t, "claude loop", run, "Repeat forever: "+gatewayLiveLoop)
		expectInterrupted(t, "claude loop", calls, added, out, `"is_error":true`)
	})

	codexEnv := func(t *testing.T, cwd string) []string {
		env, err := s.execEnv(codexSess)
		if err != nil {
			t.Fatal(err)
		}
		return gatewayLiveEnv(t, cwd, append(env, "CODEX_HOME="+codexHome, "OPENAI_API_KEY="))
	}
	t.Run("codex exec", func(t *testing.T) {
		if _, err := exec.LookPath("codex"); err != nil {
			t.Fatal("codex not on PATH: the gated check cannot pass without it")
		}
		argv, err := agent.ChatCommand(config.AgentCodex, "bypassPermissions", "", "gpt-5.5", "")
		if err != nil {
			t.Fatal(err)
		}
		cwd := t.TempDir()
		run := func(prompt string) (string, error) {
			events, err := gatewayLiveRun(t, argv, cwd, codexEnv(t, cwd), prompt)
			return gatewayLiveText(events), err
		}
		calls, added, out, err := check(t, "codex exec reply", run, "Reply with the fixture marker.")
		expectReply(t, "codex exec reply", calls, added, out, err, "/v1/responses", "Bearer codex-key")
		calls, added, out, _ = check(t, "codex exec loop", run, "Repeat forever: "+gatewayLiveLoop)
		expectInterrupted(t, "codex exec loop", calls, added, out, "[agentbox.loop-guard]", "turn.failed")
	})

	t.Run("codex app-server", func(t *testing.T) {
		if _, err := exec.LookPath("codex"); err != nil {
			t.Fatal("codex not on PATH: the gated check cannot pass without it")
		}
		cwd := t.TempDir()
		run := func(prompt string) (string, error) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "codex", "app-server")
			cmd.Dir, cmd.Env = cwd, codexEnv(t, cwd)
			stdin, _ := cmd.StdinPipe()
			stdout, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { stdin.Close(); _ = cmd.Wait() }()
			var b strings.Builder
			err := agent.RunCodexTurn(ctx, stdin, stdout, func() { stdin.Close() }, nil,
				agent.CodexTurn{Prompt: prompt, Model: "gpt-5.5", Cwd: cwd}, func(line []byte) { b.Write(line); b.WriteByte('\n') })
			return b.String(), err
		}
		calls, added, out, err := check(t, "codex app-server reply", run, "Reply with the fixture marker.")
		expectReply(t, "codex app-server reply", calls, added, out, err, "/v1/responses", "Bearer codex-key")
		calls, added, out, _ = check(t, "codex app-server loop", run, "Repeat forever: "+gatewayLiveLoop)
		expectInterrupted(t, "codex app-server loop", calls, added, out, "[agentbox.loop-guard]", "turn.failed")
	})
}
