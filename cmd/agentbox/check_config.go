package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/diagnostics"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

func checkConfig(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("check-config", flag.ContinueOnError)
	path := flags.String("config", "config.json", "configuration path")
	environment := flags.Bool("environment", false, "also check Docker, image, accounts and data directory with an isolated temporary write probe")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, loadErr := config.Load(*path)
	if loadErr != nil && !*environment {
		return loadErr
	}
	report := diagnostics.Offline(loadErr == nil)
	var interrupted error
	if *environment && loadErr == nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var inspector diagnostics.Runtime
		client, err := dockerx.NewInspector()
		if err == nil {
			defer client.Close()
			inspector = client
		}
		report = diagnostics.Instance(ctx, cfg, inspector, true)
		interrupted = ctx.Err()
		if err != nil { // Client configuration failure is an observed failure, not a skip.
			for i, c := range report.Checks {
				if c.ID == "docker" {
					report.Checks[i] = diagnostics.Result("docker", diagnostics.Failed, "docker_unavailable")
				}
			}
		}
	}
	result := struct {
		Valid       bool               `json:"valid"`
		Schema      int                `json:"schema_version"`
		Epoch       int                `json:"compatibility_epoch"`
		Environment diagnostics.Report `json:"environment"`
	}{loadErr == nil, store.SchemaVersion, 1, report}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return err
	}
	if interrupted != nil {
		return errors.New("checks interrupted; see the diagnostic JSON report")
	}
	if report.HasFailures() {
		return errors.New("checks failed; see the diagnostic JSON report")
	}
	return nil
}
