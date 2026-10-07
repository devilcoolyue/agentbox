package chat

import (
	"context"
	"errors"
	"testing"

	"agentbox/internal/agent"
	"agentbox/internal/dockerx"
)

type refusingBackend struct{ calls int }

func (b *refusingBackend) ExecStream(context.Context, string, []string, []string) (*dockerx.Stream, error) {
	b.calls++
	return nil, errors.New("synthetic exec failure")
}
func (*refusingBackend) ExitCode(context.Context, string) (int, error) {
	panic("no stream was started")
}

func TestChatExecutorAdmissionAndCancellation(t *testing.T) {
	denied := errors.New("authorization revoked")
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			backend := &refusingBackend{}
			adapter, err := agent.Lookup(provider)
			if err != nil {
				t.Fatal(err)
			}
			environments := 0
			executor := Executor{Backend: backend, Environment: func() ([]string, error) { environments++; return nil, denied }, Interrupted: func() bool { return false }, Log: func(string, ...any) {}}
			if err := executor.Run(t.Context(), adapter, Turn{}); !errors.Is(err, denied) {
				t.Fatal(err)
			}
			if backend.calls != 0 || environments != 1 {
				t.Fatal("exec or fallback bypassed authorization")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := executor.Run(ctx, adapter, Turn{}); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if environments != 1 {
				t.Fatal("canceled work reached environment/exec")
			}
		})
	}
}

func TestChatExecutorExecDoesNotSilentlyDropOptions(t *testing.T) {
	adapter, _ := agent.Lookup("claude")
	backend := &refusingBackend{}
	executor := Executor{Backend: backend, Interrupted: func() bool { return false }}
	if err := executor.Run(t.Context(), adapter, Turn{Options: agent.TurnOptions{Unsupported: true}}); !errors.Is(err, ErrOptions) {
		t.Fatal(err)
	}
	if backend.calls != 0 {
		t.Fatal("unsupported options launched an exec")
	}
}
