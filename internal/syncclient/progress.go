package syncclient

import (
	"context"
	"io"
	"sync"
	"time"
)

// Progress is observational only: bytes read are not proof of publication, and
// verified operations are not a committed baseline. Only the command result is
// authoritative. Path is always project-relative, never a local absolute path.
type Progress struct {
	Sequence     uint64 `json:"sequence"`
	Stage        string `json:"stage"`
	Path         string `json:"path"`
	Operation    string `json:"operation"`
	Completed    int    `json:"completed"`
	Total        int    `json:"total"`
	Bytes        int64  `json:"bytes"`
	TotalBytes   int64  `json:"total_bytes"`
	FileBytes    int64  `json:"file_bytes"`
	FileTotal    int64  `json:"file_total"`
	Scanned      int    `json:"scanned"`
	ScannedBytes int64  `json:"scanned_bytes"`
}
type progressKey struct{}
type progressState struct {
	mu         sync.Mutex
	value      Progress
	generation uint64
	observe    func(Progress)
}

// WithProgress adds an optional synchronous observer. It must return promptly;
// desktop IPC uses a single coalesced snapshot, never writes from the executor.
func WithProgress(ctx context.Context, observe func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, &progressState{observe: observe})
}
func progressStateFor(ctx context.Context) *progressState {
	p, _ := ctx.Value(progressKey{}).(*progressState)
	return p
}
func changeProgress(ctx context.Context, change func(*progressState)) {
	p := progressStateFor(ctx)
	if p == nil || ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	change(p)
	p.value.Sequence++
	if p.observe != nil {
		p.observe(p.value)
	}
}
func progressStage(ctx context.Context, stage string) {
	changeProgress(ctx, func(p *progressState) {
		p.generation++
		p.value.Stage = stage
		p.value.Path = ""
		p.value.Operation = ""
		p.value.FileBytes = 0
		p.value.FileTotal = 0
		p.value.Scanned = 0
		p.value.ScannedBytes = 0
	})
}
func progressPlan(ctx context.Context, operations []Operation) {
	changeProgress(ctx, func(p *progressState) {
		p.value.Total = len(operations)
		p.value.Completed = 0
		p.value.Bytes = 0
		p.value.TotalBytes = 0
		for _, op := range operations {
			if (op.Kind == "upload" || op.Kind == "download") && op.After != nil {
				p.value.TotalBytes += op.After.Size
			}
		}
	})
}
func progressOperation(ctx context.Context, op Operation) {
	changeProgress(ctx, func(p *progressState) {
		p.generation++
		p.value.Stage = "applying"
		p.value.Path = op.Path
		p.value.Operation = op.Kind
		p.value.FileBytes = 0
		p.value.FileTotal = 0
		if (op.Kind == "upload" || op.Kind == "download") && op.After != nil {
			p.value.FileTotal = op.After.Size
		}
	})
}
func progressVerified(ctx context.Context) {
	changeProgress(ctx, func(p *progressState) { p.generation++; p.value.Completed++ })
}
func progressScan(ctx context.Context, path string, files int, bytes int64) {
	changeProgress(ctx, func(p *progressState) { p.value.Path = path; p.value.Scanned = files; p.value.ScannedBytes = bytes })
}

type progressReader struct {
	reader     io.Reader
	ctx        context.Context
	generation uint64
}

func trackProgress(ctx context.Context, reader io.Reader) io.Reader {
	p := progressStateFor(ctx)
	if p == nil {
		return reader
	}
	p.mu.Lock()
	generation := p.generation
	p.mu.Unlock()
	return &progressReader{reader: reader, ctx: ctx, generation: generation}
}
func (r *progressReader) Read(b []byte) (int, error) {
	n, err := r.reader.Read(b)
	if n > 0 {
		changeProgress(r.ctx, func(p *progressState) {
			if p.generation != r.generation {
				return
			}
			amount := min(int64(n), p.value.FileTotal-p.value.FileBytes)
			p.value.FileBytes += amount
			p.value.Bytes += amount
		})
	}
	return n, err
}

// One latest snapshot, up to ten messages/second plus a final flush. Producers
// never wait for a slow pipe. Shutdown joins the pump before the result is sent.
type progressPump struct {
	mu     sync.Mutex
	latest *Progress
	closed bool
	stop   chan struct{}
	done   chan struct{}
}

func startProgressPump(ctx context.Context, emit func(Progress)) *progressPump {
	p := &progressPump{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		flush := func() {
			p.mu.Lock()
			value := p.latest
			p.latest = nil
			p.mu.Unlock()
			if value != nil && ctx.Err() == nil {
				emit(*value)
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				flush()
			case <-p.stop:
				flush()
				return
			}
		}
	}()
	return p
}
func (p *progressPump) offer(value Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.latest = &value
	}
}
func (p *progressPump) finish() { p.mu.Lock(); p.closed = true; p.mu.Unlock(); close(p.stop); <-p.done }
