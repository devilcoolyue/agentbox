package syncclient

import (
	"context"
	"sync"
	"time"

	"agentbox/internal/syncproto"
)

// Local deadlines start before each request, avoiding dependence on server clock
// offset. The v1 lease TTL is 30s; a 2s margin cancels work before server expiry.
// A distributed lease does not lock external editors or make local writes a
// transaction. Every local publication also calls Guard and checks its context.
type leaseKeeper struct {
	remote   *Remote
	grant    syncproto.Lease
	ctx      context.Context
	cancel   context.CancelCauseFunc
	mu       sync.Mutex
	renewMu  sync.Mutex
	deadline time.Time
	timer    *time.Timer
	epoch    uint64
	closed   bool
	done     chan struct{}
}

func startLease(ctx context.Context, remote *Remote, binding syncproto.Binding, device string) (*leaseKeeper, error) {
	started := time.Now()
	grant, err := remote.Acquire(ctx, binding.Workspace, binding.Project, device)
	if err != nil {
		return nil, err
	}
	work, cancel := context.WithCancelCause(ctx)
	k := &leaseKeeper{remote: remote, grant: grant, ctx: work, cancel: cancel, done: make(chan struct{})}
	if grant.Path != binding.ProjectPath || !time.Now().Before(started.Add(syncproto.LeaseTTL-2*time.Second)) {
		cancel(ErrBinding)
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = remote.Release(cleanup, grant)
		return nil, ErrBinding
	}
	k.arm(started)
	go func() {
		defer close(k.done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-ticker.C:
				if k.Guard(work) != nil {
					return
				}
			}
		}
	}()
	return k, nil
}
func (k *leaseKeeper) arm(start time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	k.deadline = start.Add(syncproto.LeaseTTL - 2*time.Second)
	k.epoch++
	epoch := k.epoch
	if k.timer != nil {
		k.timer.Stop()
	}
	k.timer = time.AfterFunc(time.Until(k.deadline), func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		if !k.closed && k.epoch == epoch {
			k.cancel(syncproto.ErrLeaseExpired)
		}
	})
}
func (k *leaseKeeper) Guard(ctx context.Context) error {
	k.renewMu.Lock()
	defer k.renewMu.Unlock()
	if err := context.Cause(k.ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	k.mu.Lock()
	deadline := k.deadline
	k.mu.Unlock()
	if !time.Now().Before(deadline) {
		k.cancel(syncproto.ErrLeaseExpired)
		return syncproto.ErrLeaseExpired
	}
	call, stop := context.WithDeadline(k.ctx, deadline)
	defer stop()
	started := time.Now()
	_, err := k.remote.Renew(call, k.grant)
	if err != nil {
		k.cancel(err)
		return err
	}
	if err = context.Cause(k.ctx); err != nil {
		return err
	}
	k.arm(started)
	return nil
}
func (k *leaseKeeper) Close() {
	k.cancel(context.Canceled)
	k.mu.Lock()
	k.closed = true
	if k.timer != nil {
		k.timer.Stop()
	}
	k.mu.Unlock()
	<-k.done
	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_ = k.remote.Release(ctx, k.grant) // Expiration also fences cleanup after disconnect.
}
