package chat

import (
	"agentbox/internal/agent"
	"agentbox/internal/dockerx"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// interruptGrace is how long a protocol interrupt may take before the turn
// falls back to SIGINT, in case the CLI stopped reading its input.
var interruptGrace = 10 * time.Second

// Sent instead of a question the page could not show; the model asks in text.
const questionUnshownMessage = "The question could not be shown to the user (malformed or too long). Ask it in plain text instead."

var errInputClosed = errors.New("agent input closed")

// controlInput serializes writes to the CLI's stdin: the prompt, answers
// from other goroutines and interrupts. Nothing is written after close.
type controlInput struct {
	mu     sync.Mutex
	stream *dockerx.Stream
	closed bool
}

func (c *controlInput) send(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errInputClosed
	}
	_, err := c.stream.Write(p)
	return err
}

func (c *controlInput) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		_ = c.stream.CloseWrite()
	}
}

// interactiveTurn runs a Claude turn whose stdin stays open as stream-json:
// the prompt goes in as a user message, AskUserQuestion permission requests
// are answered through Ask, and the result event closes stdin so the
// one-turn process exits as before (after any next-prompt suggestion).
func (e Executor) interactiveTurn(ctx context.Context, turn Turn, cmd []string) error {
	env, err := e.Environment()
	if err != nil {
		return err
	}
	stream, err := e.Backend.ExecStream(ctx, turn.ContainerID, cmd, env)
	if err != nil {
		return errors.New("exec失败: " + err.Error())
	}
	defer stream.Close()
	var ended atomic.Bool
	defer ended.Store(true)

	in := &controlInput{stream: stream}
	if err := in.send(agent.ClaudeUserMessage(turn.Text)); err != nil {
		return errors.New("写入提示词失败: " + err.Error())
	}
	// Interrupt in protocol while stdin is open. The handler is removed under
	// the room's lock before stdin closes, so a later interrupt takes the
	// SIGINT path rather than a write that cannot land.
	e.SetStop(func() {
		_ = in.send(agent.ClaudeInterrupt("agentbox-interrupt"))
		if e.Signal != nil {
			time.AfterFunc(interruptGrace, func() {
				if !ended.Load() {
					e.Signal()
				}
			})
		}
	})
	finishInput := sync.OnceFunc(func() { e.SetStop(nil); in.close() })
	defer finishInput()

	asks, cancelAsks := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancelAsks(); wg.Wait() }()

	pr, pw := io.Pipe()
	defer pr.Close() // 提前返回时解开 demux 的阻塞写
	stderr := &tailBuffer{max: 8 << 10}
	demuxDone := make(chan struct{})
	go func() {
		defer close(demuxDone)
		err := stream.Demux(pw, stderr)
		pw.CloseWithError(err)
	}()
	finishOutput := func() { stream.Close(); _ = pr.Close(); <-demuxDone }
	defer finishOutput()

	pending := map[string]context.CancelFunc{}
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 1<<20), 32<<20) // single events can be large
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if c, ok := agent.ParseClaudeControl(line); ok {
			e.control(asks, &wg, in, pending, c)
			continue
		}
		e.OnLine(line)
		if agent.IsClaudeResult(line) {
			finishInput()
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取 agent 输出失败: %w", err)
	}

	code, err := e.Backend.ExitCode(ctx, stream.ExecID)
	if err != nil {
		return errors.New("等待退出码失败: " + err.Error())
	}
	if code != 0 {
		if code == 130 {
			return ErrInterrupted
		}
		return &ExitError{Code: code, Stderr: stderr.String()}
	}
	return nil
}

// control answers one control message. Questions wait in their own goroutine
// so the stream keeps flowing; the CLI may withdraw one (control_cancel_request)
// when it is interrupted.
func (e Executor) control(ctx context.Context, wg *sync.WaitGroup, in *controlInput, pending map[string]context.CancelFunc, c agent.Control) {
	switch c.Type {
	case "control_cancel_request":
		if cancel := pending[c.RequestID]; cancel != nil {
			cancel()
		}
	case "control_request":
		if c.Subtype != "can_use_tool" {
			_ = in.send(agent.ClaudeUnsupported(c.RequestID))
			return
		}
		if c.ToolName != agent.AskUserQuestionTool || e.Ask == nil {
			_ = in.send(agent.ClaudeDeny(c.RequestID, agent.PermissionDeniedMessage))
			return
		}
		q, err := agent.ClaudeQuestion(c)
		if err != nil {
			e.Log("chat question not shown: %v", err)
			_ = in.send(agent.ClaudeDeny(c.RequestID, questionUnshownMessage))
			return
		}
		qctx, cancel := context.WithCancel(ctx)
		pending[c.RequestID] = cancel
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			reply, err := q.Reply(e.Ask(qctx, q))
			if err != nil {
				e.Log("chat question reply: %v", err)
				reply = agent.ClaudeDeny(c.RequestID, questionUnshownMessage)
			}
			if reply != nil {
				_ = in.send(reply)
			}
		}()
	}
	// control_response acknowledges our own requests; nothing waits on it.
}
