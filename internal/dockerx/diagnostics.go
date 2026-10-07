package dockerx

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/docker/docker/client"
)

// Inspector opens only the Docker client. It does not ping until requested,
// so an unavailable daemon can be represented as a diagnostic result.
type Inspector struct{ cli *client.Client }

func NewInspector() (*Inspector, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Inspector{cli: cli}, nil
}
func (i *Inspector) Close() error                             { return i.cli.Close() }
func (i *Inspector) DiagnosticPing(ctx context.Context) error { _, err := i.cli.Ping(ctx); return err }
func (i *Inspector) DiagnosticImage(ctx context.Context, ref string) error {
	return diagnosticImage(ctx, i.cli, ref)
}
func (m *Manager) DiagnosticPing(ctx context.Context) error { _, err := m.cli.Ping(ctx); return err }
func (m *Manager) DiagnosticImage(ctx context.Context, ref string) error {
	return diagnosticImage(ctx, m.cli, ref)
}
func diagnosticImage(ctx context.Context, cli *client.Client, ref string) error {
	_, err := cli.ImageInspect(ctx, ref)
	if client.IsErrNotFound(err) {
		return fmt.Errorf("%w: %w", fs.ErrNotExist, err)
	}
	return err
}
