package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agentbox/internal/backup"
	"agentbox/internal/config"
	"agentbox/internal/store"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Maintenance commands run before config/server initialization: verification
// and restore must work without Docker, credentials or a running service.
func maintenance(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "git-key-rotate":
		return rotateGitKeys(ctx, args[1:])
	case "check-config":
		path := flags.String("config", "config.json", "configuration path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		if _, err := config.Load(*path); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"valid": true, "schema_version": store.SchemaVersion, "compatibility_epoch": 1})
	case "backup":
		cfg := flags.String("config", "config.json", "configuration path")
		output := flags.String("output", "", "new backup file (default: data_dir/backups)")
		full := flags.Bool("full", false, "include all user data; requires stopped service and containers")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected backup arguments")
		}
		if *output == "" {
			raw, err := os.ReadFile(*cfg)
			if err != nil {
				return err
			}
			var c struct {
				DataDir string `json:"data_dir"`
			}
			if err = json.Unmarshal(raw, &c); err != nil {
				return err
			}
			if c.DataDir == "" {
				c.DataDir = "data"
			}
			if !filepath.IsAbs(c.DataDir) {
				c.DataDir = filepath.Join(filepath.Dir(*cfg), c.DataDir)
			}
			dest := filepath.Join(c.DataDir, "backups")
			if err = os.MkdirAll(dest, 0o700); err != nil {
				return err
			}
			kind := "system"
			if *full {
				kind = "full"
			}
			*output = filepath.Join(dest, "agentbox-backup-"+kind+"-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".tar.gz")
		}
		m, err := backup.Create(ctx, backup.Options{Config: *cfg, Output: *output, Full: *full, CheckStopped: checkBackupContainers})
		if err != nil {
			return err
		}
		hash, err := archiveHash(*output)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"archive": *output, "sha256": hash, "mode": m.Mode, "entries": len(m.Entries), "consistency": m.Consistency})
	case "backup-verify":
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: agentbox backup-verify ARCHIVE")
		}
		m, err := backup.Verify(ctx, flags.Arg(0))
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"valid": true, "format_version": m.Version, "mode": m.Mode, "created": m.Created, "entries": len(m.Entries)})
	case "restore":
		target := flags.String("to", "", "new instance directory (must not exist)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || *target == "" {
			return errors.New("usage: agentbox restore --to NEW_DIRECTORY ARCHIVE")
		}
		m, err := backup.Restore(ctx, flags.Arg(0), *target)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"restored": *target, "mode": m.Mode, "note": "verify credentials and container mounts before starting; system backups omit workspaces"})
	}
	return errors.New("unknown maintenance command")
}

func archiveHash(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checkBackupContainers(ctx context.Context, roots []string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	canonical := make([]string, 0, len(roots))
	for _, root := range roots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return fmt.Errorf("resolve backup source: %w", err)
		}
		canonical = append(canonical, resolved)
	}
	list, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return fmt.Errorf("cannot verify stopped containers: %w", err)
	}
	for _, c := range list {
		for _, mount := range c.Mounts {
			if mount.Source == "" {
				continue
			}
			for _, root := range canonical {
				if overlaps(root, mount.Source) {
					return fmt.Errorf("stop container %s before full backup: it mounts backup data", c.ID[:12])
				}
			}
		}
	}
	return nil
}

func overlaps(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator)) || a == "/" || b == "/"
}
