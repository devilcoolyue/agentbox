package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"agentbox/internal/agentprobe"
	"agentbox/internal/config"
	"agentbox/internal/dockerx"
)

// This flag-form maintenance command is intentional: an old binary rejects an
// unknown flag before initialization, rather than starting a server by mistake.
func checkAgentImage(ctx context.Context, args []string, out io.Writer, probe func(context.Context, string) (agentprobe.Report, error)) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: agentbox --check-agent-image <local-image>")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	report, err := probe(ctx, args[0])
	if writeErr := json.NewEncoder(out).Encode(report); writeErr != nil {
		return writeErr
	}
	if err != nil {
		return err
	}
	if !report.Passed(report.ImageID) {
		return errors.New("候选镜像验证未通过")
	}
	return nil
}
func probeAgentImage(ctx context.Context, ref string) (agentprobe.Report, error) {
	m, err := dockerx.New(&config.Config{})
	if err != nil {
		return agentprobe.Report{}, err
	}
	defer m.Close()
	img, err := m.InspectCLIImage(ctx, ref)
	if err != nil {
		return agentprobe.Report{}, err
	}
	return m.ValidateCLIImage(ctx, img.ID)
}
