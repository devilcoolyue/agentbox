package dockerx

import (
	"context"
	"errors"
	"io"
)

// ExecOutput streams bounded binary stdout; stderr can never enter an archive.
// The argv supplied by gitx includes an in-container process-group timeout,
// because closing the Docker attachment alone is not a process kill.
func (m *Manager) ExecOutput(ctx context.Context, cid string, cmd []string, dst io.Writer, maxBytes int64) error {
	if maxBytes < 1 || maxBytes > 1<<40 {
		return errors.New("invalid output bound")
	}
	stream, err := m.ExecStream(ctx, cid, cmd, nil)
	if err != nil {
		return err
	}
	defer stream.Close()
	if err = stream.CloseWrite(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	out := &boundedOutputWriter{dst: dst, left: maxBytes}
	if err = stream.Demux(out, io.Discard); err != nil {
		return err
	}
	code, err := m.ExitCode(ctx, stream.ExecID)
	if err != nil {
		return err
	}
	if code != 0 {
		return errors.New("container archive command failed")
	}
	return ctx.Err()
}

type boundedOutputWriter struct {
	dst  io.Writer
	left int64
}

func (w *boundedOutputWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		return 0, errors.New("container output exceeds limit")
	}
	n, err := w.dst.Write(p)
	w.left -= int64(n)
	return n, err
}
