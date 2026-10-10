package dockerx

import (
	"context"
	"io"
)

type stderrLinesKey struct{}

// WithStderrLines streams each stderr segment of a cancelable command to fn
// while it runs. Git redraws its --progress meters with carriage returns;
// those redraws go only to fn and are kept out of the bounded error output,
// so a long transfer cannot push the real failure message past the limit.
// Segments ending in a newline still reach the error output as before.
func WithStderrLines(ctx context.Context, fn func(string)) context.Context {
	return context.WithValue(ctx, stderrLinesKey{}, fn)
}

func stderrLines(ctx context.Context) func(string) {
	fn, _ := ctx.Value(stderrLinesKey{}).(func(string))
	return fn
}

// progressWriter splits stderr on CR and LF. A segment is buffered up to the
// error output limit (the plain writer kept no more either) and handed to the
// callback truncated, never buffered unbounded.
type progressWriter struct {
	next io.Writer
	fn   func(string)
	line []byte
}

const (
	progressLineLimit     = 8 << 10
	progressCallbackLimit = 512
)

func (w *progressWriter) Write(p []byte) (int, error) {
	start := 0
	for i, b := range p {
		if b != '\r' && b != '\n' {
			continue
		}
		w.append(p[start:i])
		w.emit()
		if b == '\n' {
			_, _ = w.next.Write(append(w.line, '\n'))
		}
		w.line = w.line[:0]
		start = i + 1
	}
	w.append(p[start:])
	return len(p), nil
}

func (w *progressWriter) append(p []byte) {
	if room := progressLineLimit - len(w.line); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		w.line = append(w.line, p...)
	}
}

// flush keeps an unterminated final segment (a crash mid-line) in the error
// output, as the plain writer would have.
func (w *progressWriter) flush() {
	if len(w.line) > 0 {
		w.emit()
		_, _ = w.next.Write(w.line)
		w.line = w.line[:0]
	}
}

func (w *progressWriter) emit() {
	line := w.line
	if len(line) > progressCallbackLimit {
		line = line[:progressCallbackLimit]
	}
	if len(line) > 0 {
		w.fn(string(line))
	}
}
