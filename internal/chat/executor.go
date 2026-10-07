// Package chat owns CLI transport selection and stream lifetime. Admission,
// history and settlement stay with the caller; it never schedules another turn.
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
	"strconv"
	"sync"
)

var ErrInterrupted = errors.New("agent exited after interrupt")
var ErrOptions = errors.New("exec fallback cannot preserve requested options")

type Backend interface {
	ExecStream(context.Context, string, []string, []string) (*dockerx.Stream, error)
	ExitCode(context.Context, string) (int, error)
}

type Turn struct {
	SessionID, ContainerID, Text, ChatID, Model, Effort, Permission string
	Options                                                         agent.TurnOptions
}

// Environment rechecks authorization immediately before EACH Docker exec.
// Callbacks belong to a single turn; OnLine is called serially, and SetStop
// registers/removes the protocol interrupt handler under the room owner's lock.
type Executor struct {
	Backend     Backend
	Environment func() ([]string, error)
	SetStop     func(func())
	Interrupted func() bool
	OnLine      func([]byte)
	Log         func(string, ...any)
}

func (e Executor) Run(ctx context.Context, adapter agent.Adapter, turn Turn) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if adapter.Capabilities().AppServer {
		fallback, err := e.appServerTurn(ctx, turn)
		switch {
		case err == nil:
			return nil
		case !fallback:
			return err
		default:
			e.Log("codex app-server 不可用，回退 exec (%s): %v", turn.SessionID, err)
		}
	}
	// Only a failure before a turn starts permits fallback. Cancellation during
	// the handshake must never launch a new exec process.
	if e.Interrupted() || ctx.Err() != nil {
		return context.Canceled
	}
	if turn.Options.Unsupported {
		return ErrOptions
	}
	cmd, err := adapter.Chat(turn.Permission, turn.ChatID, turn.Model, turn.Effort, turn.Options.Control)
	if err != nil {
		return err
	}
	return e.execTurn(ctx, turn, cmd)
}

// execTurn 跑传统的一次性 CLI 回合：stdin 喂提示词后关闭，stdout 收 JSONL
// 事件直到进程退出。claude 一直走这里；codex 仅在 app-server 不可用时回退。
func (e Executor) execTurn(ctx context.Context, turn Turn, cmd []string) error {
	env, err := e.Environment()
	if err != nil {
		return err
	}
	stream, err := e.Backend.ExecStream(ctx, turn.ContainerID, cmd, env)
	if err != nil {
		return errors.New("exec失败: " + err.Error())
	}
	defer stream.Close()

	if _, err := stream.Write([]byte(turn.Text)); err != nil {
		return errors.New("写入提示词失败: " + err.Error())
	}
	if err := stream.CloseWrite(); err != nil {
		return errors.New("关闭stdin失败: " + err.Error())
	}

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

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 1<<20), 32<<20) // single events can be large
	for scanner.Scan() {
		e.OnLine(bytes.TrimSpace(scanner.Bytes()))
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
		return errors.New("agent 进程退出码 " + strconv.Itoa(code) + ": " + stderr.String())
	}
	return nil
}

// appServerTurn 经 codex app-server 协议跑一回合，事件由协议驱动器翻译后
// 进 onLine（含打字机用的流式增量）。返回 fallback=true 表示回合尚未开始
// （握手/开线程失败），可安全改走传统 exec 路径。
func (e Executor) appServerTurn(ctx context.Context, turn Turn) (fallback bool, err error) {
	env, err := e.Environment()
	if err != nil {
		return false, err
	}
	stream, err := e.Backend.ExecStream(ctx, turn.ContainerID, agent.AppServerCommand(), env)
	if err != nil {
		return false, errors.New("exec失败: " + err.Error())
	}
	defer stream.Close()

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

	// 中断优先走协议内的 turn/interrupt（回合优雅收尾、不惊动进程），
	// SIGINT 仅在协议没接管时兜底（见 interrupt）。
	ich := make(chan struct{})
	var once sync.Once
	e.SetStop(func() { once.Do(func() { close(ich) }) })
	defer e.SetStop(nil)

	err = agent.RunCodexTurn(ctx, stream, pr,
		func() { _ = stream.CloseWrite() }, // 硬断兜底：app-server 随 stdin EOF 退出
		ich,
		agent.CodexTurn{Prompt: turn.Text, ThreadID: turn.ChatID, Model: turn.Model, Effort: turn.Effort, Cwd: dockerx.WorkspaceMount, RejectInheritedEffort: turn.Options.Unsupported},
		e.OnLine)
	if err != nil {
		if errors.Is(err, agent.ErrAppServerUnavailable) {
			return true, err
		}
		finishOutput()
		if tail := stderr.String(); tail != "" {
			return false, fmt.Errorf("%v: %s", err, tail)
		}
		return false, err
	}

	_ = stream.CloseWrite() // 正常收尾：关 stdin 让 app-server 退出
	drainDone := make(chan struct{})
	go func() { defer close(drainDone); _, _ = io.Copy(io.Discard, pr) }()
	defer func() { finishOutput(); <-drainDone }()
	if code, err := e.Backend.ExitCode(ctx, stream.ExecID); err != nil {
		e.Log("codex app-server 等退出码 %s: %v", turn.SessionID, err)
	} else if code != 0 {
		finishOutput()
		// 回合已完整走完，退出码异常只记日志不打扰用户
		e.Log("codex app-server 退出码 %d (%s): %s", code, turn.SessionID, stderr.String())
	}
	return false, nil
}

// tailBuffer keeps only the last max bytes written (stderr diagnostics).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
