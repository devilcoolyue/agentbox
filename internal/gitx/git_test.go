package gitx

import (
	"context"
	"errors"
	"slices"
	"testing"
)

type executorFunc func(context.Context, string, []string) (string, error)

func (f executorFunc) ExecCommand(ctx context.Context, id string, cmd []string) (string, error) {
	return f(ctx, id, cmd)
}

func TestRunnerRejectsTraversalBeforeStartingSession(t *testing.T) {
	r := New(executorFunc(func(context.Context, string, []string) (string, error) {
		t.Fatal("executed invalid path")
		return "", nil
	}), func(context.Context, string) (string, func(), error) {
		t.Fatal("started session for invalid path")
		return "", nil, nil
	})
	for _, repo := range []string{"/etc", "../other", "project/../../other", "a/../b", "a\\b", "a\x00b"} {
		if _, err := r.Run(t.Context(), "s1", repo, "status"); err == nil {
			t.Errorf("accepted %q", repo)
		}
	}
}

func TestRunnerPreservesArgumentsAndReleasesOnFailure(t *testing.T) {
	var released bool
	wantErr := errors.New("runtime unavailable")
	message := "message with 'quotes' $(touch /tmp/never)\nsecond line"
	r := New(executorFunc(func(_ context.Context, id string, cmd []string) (string, error) {
		if id != "container-1" || !slices.Equal(cmd[len(cmd)-3:], []string{"commit", "-m", message}) {
			t.Fatalf("incorrect container/arguments: %s %q", id, cmd)
		}
		if !slices.Contains(cmd, "/workspace/project with spaces") || cmd[0] != "/usr/bin/timeout" {
			t.Fatalf("missing container path/timeout: %q", cmd)
		}
		if !slices.Contains(cmd, "-i") || !slices.Contains(cmd, "core.hooksPath=/dev/null") {
			t.Fatalf("missing environment/hook isolation: %q", cmd)
		}
		return "", wantErr
	}), func(_ context.Context, id string) (string, func(), error) {
		if id != "s1" {
			t.Fatalf("wrong session %s", id)
		}
		return "container-1", func() { released = true }, nil
	})
	_, err := r.Run(t.Context(), "s1", "project with spaces", "commit", "-m", message)
	if !errors.Is(err, wantErr) || !released {
		t.Fatalf("error=%v released=%v", err, released)
	}
}

func TestRunnerFailsClosed(t *testing.T) {
	var r *Runner
	if _, err := r.Run(t.Context(), "s1", "", "status"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing runtime: %v", err)
	}
	denied := errors.New("quota denied")
	r = New(executorFunc(func(context.Context, string, []string) (string, error) {
		t.Fatal("executed after prepare denied access")
		return "", nil
	}), func(context.Context, string) (string, func(), error) {
		return "", nil, denied
	})
	if _, err := r.Run(t.Context(), "s1", "", "status"); !errors.Is(err, denied) {
		t.Fatalf("prepare error: %v", err)
	}
}
