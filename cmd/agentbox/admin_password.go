package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/password"
	"agentbox/internal/store"
	"github.com/moby/term"
	"golang.org/x/sys/unix"
)

func resetAdminPassword(ctx context.Context, args []string, output, diagnostics io.Writer, readPassword func(context.Context) ([]byte, error)) error {
	flags := flag.NewFlagSet("admin-reset-password", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	cfgPath := flags.String("config", "", "required: configuration of the stopped instance")
	user := flags.String("user", "", "required: existing administrator username")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *cfgPath == "" || *user == "" {
		return errors.New("usage: agentbox admin-reset-password --config CONFIG --user ADMIN (interactive terminal required)")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	absConfig, err := filepath.Abs(*cfgPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(absConfig)
	if err != nil {
		return err
	}
	lockedDir := cfg.DataDir
	lock, err := os.OpenFile(filepath.Join(lockedDir, "agentbox.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("stop agentbox before resetting its administrator password; cannot lock data directory: %w", err)
	}
	// Reload under the same lock used by startup. Never reset a different
	// instance if an operator changed data_dir while we were acquiring it.
	checkConfig := func() error {
		current, err := config.Load(absConfig)
		if err != nil {
			return err
		}
		if current.DataDir != lockedDir {
			return errors.New("configuration data_dir changed; run recovery again with the correct configuration")
		}
		return nil
	}
	if err := checkConfig(); err != nil {
		return err
	}
	dbPath := filepath.Join(lockedDir, "state.db")
	st, err := store.OpenForMaintenance(dbPath)
	if err != nil {
		return fmt.Errorf("open existing instance database: %w", err)
	}
	defer st.Close()
	admin, ok := st.GetUser(*user)
	if !ok || admin.Role != store.RoleAdmin {
		return errors.New("target must be an existing administrator; no account was created or promoted")
	}
	if _, err := fmt.Fprintf(diagnostics, "Configuration: %s\nDatabase: %s\nAdministrator: %s\nAll login tokens for this administrator will be revoked.\n", absConfig, dbPath, admin.Name); err != nil {
		return err
	}
	pw, err := readPassword(ctx)
	defer clear(pw)
	if err != nil {
		return err
	}
	if len(pw) < 8 || len(pw) > 1024 {
		return errors.New("password must be 8–1024 bytes")
	}
	if err := checkConfig(); err != nil {
		return err
	}
	hash := password.Hash(string(pw))
	if err := ctx.Err(); err != nil {
		return err
	}
	changed, err := st.ResetPasswordIfUserUnchanged(admin, hash, "")
	if err != nil {
		return fmt.Errorf("administrator recovery failed: %w", err)
	}
	if !changed {
		return errors.New("administrator identity changed; password was not reset")
	}
	return json.NewEncoder(output).Encode(map[string]any{"user": admin.Name, "password_reset": true, "tokens_revoked": true, "note": "Start the service and sign in with the new password; reconnect or pair other clients again."})
}

// Read from the controlling terminal, never argv, environment or redirected
// stdin. Keep echo disabled for both entries, restoring it even on SIGINT or
// SIGTERM (maintenance converts those signals into context cancellation).
func readAdminPassword(ctx context.Context) (value []byte, retErr error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("an interactive terminal is required; connect with ssh -t or run in a local terminal")
	}
	defer tty.Close()
	fd := tty.Fd()
	// Darwin does not reliably support poll(2) on /dev/tty. Use nonblocking
	// reads with a cancellable delay; do not leave a blocked reader goroutine.
	if err := unix.SetNonblock(int(fd), true); err != nil {
		return nil, err
	}
	state, err := term.SaveState(fd)
	if err != nil {
		return nil, err
	}
	if err := term.DisableEcho(fd, state); err != nil {
		return nil, err
	}
	defer func() {
		// Discard unfinished input before restoring echo, so cancellation cannot
		// leave a partially typed password for the caller's shell to consume.
		retErr = errors.Join(retErr, flushPasswordInput(fd), term.RestoreTerminal(fd, state))
		if retErr != nil {
			clear(value)
			value = nil
		}
	}()
	read := func(prompt string) ([]byte, error) {
		if _, err := io.WriteString(tty, prompt); err != nil {
			return nil, err
		}
		defer io.WriteString(tty, "\n")
		return readPasswordLine(ctx, int(fd))
	}
	first, err := read("New password: ")
	if err != nil {
		return nil, err
	}
	defer clear(first)
	second, err := read("Repeat new password: ")
	if err != nil {
		return nil, err
	}
	defer clear(second)
	if !bytes.Equal(first, second) {
		return nil, errors.New("passwords do not match; nothing changed")
	}
	return bytes.Clone(first), nil
}

func readPasswordLine(ctx context.Context, fd int) ([]byte, error) {
	var line []byte
	defer func() { clear(line) }()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var b [1]byte
		n, err := unix.Read(fd, b[:])
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.EOF
		}
		if b[0] == '\n' {
			return bytes.Clone(line), nil
		}
		line = append(line, b[0])
		if len(line) > 1024 {
			return nil, errors.New("password must be at most 1024 bytes")
		}
	}
}
