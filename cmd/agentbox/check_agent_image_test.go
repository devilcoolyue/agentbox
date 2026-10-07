package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"agentbox/internal/agentprobe"
)

func TestCheckAgentImageDoesNotLoadConfiguration(t *testing.T) {
	for _, kind := range []string{"passed", "failed", "incomplete", "invalid", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			var output bytes.Buffer
			called := false
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			args := []string{"synthetic-image"}
			if kind == "invalid" {
				args = nil
			}
			err := checkAgentImage(ctx, args, &output, func(_ context.Context, ref string) (agentprobe.Report, error) {
				called = true
				if ref != "synthetic-image" {
					t.Fatal(ref)
				}
				report := agentprobe.Report{Version: 1, ImageID: "immutable-synthetic", CheckedAt: 1}
				for _, name := range agentprobe.Required {
					report.Checks = append(report.Checks, agentprobe.Check{Name: name, Passed: true})
				}
				if kind == "failed" {
					report.Failure = "usage_contract_failed"
					return report, errors.New("synthetic error")
				}
				if kind == "incomplete" {
					report.Checks = nil
				}
				return report, nil
			})
			if (err == nil) != (kind == "passed") {
				t.Fatalf("unexpected result %v", err)
			}
			if (kind == "invalid" || kind == "cancelled") == called {
				t.Fatal("invalid/cancelled input reached Docker")
			}
			if called && !strings.Contains(output.String(), "immutable-synthetic") {
				t.Fatal("missing machine evidence")
			}
		})
	}
}
