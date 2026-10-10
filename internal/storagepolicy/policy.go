// Package storagepolicy enforces operator-configured filesystem capacity rules.
package storagepolicy

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"agentbox/internal/config"
)

//go:embed quota.py
var quotaHelper string

type Usage struct {
	UsedBytes  int64 `json:"used_bytes"`
	LimitBytes int64 `json:"limit_bytes"`
	Files      int64 `json:"files"`
	FileLimit  int64 `json:"file_limit"`
}
type UserUsage struct {
	Data  Usage `json:"data"`
	Cache Usage `json:"cache"`
}

func run(ctx context.Context, input any, output any) error { return runMode(ctx, input, output, false) }
func runMode(ctx context.Context, input any, output any, adopt bool) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("project quotas require Linux")
	}
	timeout := 15 * time.Second
	if adopt {
		timeout = 10 * time.Minute
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	args := []string{"-I", "-c", quotaHelper}
	if adopt {
		args = append(args, "--adopt")
	}
	cmd := exec.CommandContext(bounded, "/usr/bin/python3", args...)
	cmd.Stdin = bytes.NewReader(raw)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("storage quota check failed: %s: %w", stderr.String(), err)
	}
	if output != nil {
		return json.Unmarshal(stdout.Bytes(), output)
	}
	return nil
}

func CheckMount(ctx context.Context, p config.StoragePolicy) error {
	if p.Mount == "" {
		return nil
	}
	return run(ctx, map[string]any{"policy": p, "check_mount": true}, nil)
}

func Ensure(ctx context.Context, p config.StoragePolicy, data, tools, user, owner string) (UserUsage, error) {
	var result UserUsage
	if p.Mount == "" {
		return result, nil
	}
	err := run(ctx, map[string]any{"policy": p, "data": data, "tools": tools, "user": user, "owner": owner}, &result)
	return result, err
}

// Adopt is only for the offline maintenance command holding the data lock and
// after verifying no containers mount data or tools. Interrupted runs resume.
func Adopt(ctx context.Context, p config.StoragePolicy, data, tools, user, owner string) error {
	return runMode(ctx, map[string]any{"policy": p, "data": data, "tools": tools, "user": user, "owner": owner}, nil, true)
}
