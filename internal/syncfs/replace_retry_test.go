package syncfs

import (
	"context"
	"errors"
	"testing"

	"agentbox/internal/syncproto"
)

func TestReplaceRetryIsBoundedAndDoesNotRetryPermanentOrValidationErrors(t *testing.T) {
	sharing := errors.New("synthetic sharing violation")
	permanent := errors.New("synthetic permanent error")
	for _, scenario := range []struct {
		name     string
		failure  error
		validate error
		attempts int
	}{
		{"sharing", sharing, nil, 6},
		{"permanent", permanent, nil, 1},
		{"stale_after_wait", sharing, syncproto.ErrChanged, 1},
		{"lease_after_wait", sharing, syncproto.ErrLeaseExpired, 1},
		{"validation_sharing", sharing, sharing, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			attempts := 0
			err := retryFileReplace(t.Context(), func() error {
				if attempts > 0 {
					return scenario.validate
				}
				return nil
			}, func() error {
				attempts++
				return scenario.failure
			}, func(err error) bool { return errors.Is(err, sharing) })
			want := scenario.failure
			if scenario.validate != nil {
				want = scenario.validate
			}
			if !errors.Is(err, want) || attempts != scenario.attempts {
				t.Fatal("unexpected retry result", err, attempts)
			}
		})
	}
}

func TestReplaceRetryCancellationInterruptsWaitAndNeverReplaysSuccess(t *testing.T) {
	sharing := errors.New("synthetic sharing violation")
	for _, cancelAfterRename := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		attempts := 0
		err := retryFileReplace(ctx, func() error { return nil }, func() error {
			attempts++
			cancel()
			if cancelAfterRename {
				return nil // Publication already succeeded: never repeat it.
			}
			return sharing
		}, func(err error) bool { return errors.Is(err, sharing) })
		cancel()
		if attempts != 1 || cancelAfterRename && err != nil || !cancelAfterRename && !errors.Is(err, context.Canceled) {
			t.Fatal("canceled wait retried or successful publication was replayed", attempts, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := retryFileReplace(ctx, func() error { t.Fatal("validated canceled operation"); return nil }, func() error { t.Fatal("published canceled operation"); return nil }, func(error) bool { return true })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
