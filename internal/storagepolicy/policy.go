// Package storagepolicy enforces operator-configured filesystem capacity rules.
package storagepolicy

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"agentbox/internal/config"
)

//go:embed quota.py
var quotaHelper string

// Rejections the helper reports for a request, not for a broken filesystem.
var (
	ErrBudget       = errors.New("allocated project quotas exceed storage budget")
	ErrPreviousData = errors.New("previous user data requires operator retirement")
)

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
		msg := stderr.String()
		switch {
		case strings.Contains(msg, ErrBudget.Error()):
			err = ErrBudget
		case strings.Contains(msg, ErrPreviousData.Error()), strings.Contains(msg, "nonempty storage root requires offline quota adoption"):
			err = ErrPreviousData
		}
		return fmt.Errorf("storage quota check failed: %s: %w", msg, err)
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

// Retire releases the limits of a deleted account's projects. Markers stay
// registered: leftover files remain visible and keep a non-empty root reserved.
func Retire(ctx context.Context, p config.StoragePolicy, data, owner string) error {
	if p.Mount == "" {
		return nil
	}
	return run(ctx, map[string]any{"policy": p, "data": data, "owner": owner, "user": "retired", "retire": true}, nil)
}

// Project is one registered quota project and its kernel-accounted usage.
type Project struct {
	Usage
	Project int64  `json:"project"`
	Kind    string `json:"kind"` // data | cache
	Owner   string `json:"owner"`
	Root    string `json:"root"`
	Retired bool   `json:"retired,omitempty"` // account deleted, limit released
}

// Charged is what a project takes from the budget: its limit, or for a
// retired project the files still left on disk. Mirrors quota.py.
func (p Project) Charged() int64 {
	if p.Retired {
		return p.UsedBytes
	}
	return p.LimitBytes
}

// Report is the kernel usage of every registered project plus project 0,
// which holds whatever lives outside user roots.
type Report struct {
	Projects   []Project `json:"projects"`
	Unassigned Usage     `json:"unassigned"`
}

// ReadReport only reads limits and usage; it takes no registry lock.
func ReadReport(ctx context.Context, p config.StoragePolicy, data string) (Report, error) {
	var result Report
	if p.Mount == "" {
		return result, nil
	}
	err := run(ctx, map[string]any{"policy": p, "data": data, "report": true}, &result)
	return result, err
}

// Adopt is only for the offline maintenance command holding the data lock and
// after verifying no containers mount data or tools. Interrupted runs resume.
func Adopt(ctx context.Context, p config.StoragePolicy, data, tools, user, owner string) error {
	return runMode(ctx, map[string]any{"policy": p, "data": data, "tools": tools, "user": user, "owner": owner}, nil, true)
}
