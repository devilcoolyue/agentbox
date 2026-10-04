package syncfs

import (
	"context"
	"time"
)

// Used only by Windows publication. Up to six atomic rename attempts span at
// most 575 ms of waits; the caller's context also bounds validation and waits.
// Revalidation errors are never retried. No fallback removes the destination,
// and the staged file and durable recovery reference are reused unchanged.
func retryFileReplace(ctx context.Context, validate, rename func() error, retryable func(error) bool) error {
	delays := [...]time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validate(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		err := rename()
		if err == nil || !retryable(err) || attempt == len(delays) {
			return err
		}
		timer := time.NewTimer(delays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
