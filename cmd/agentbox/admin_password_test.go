package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentbox/internal/password"
	"agentbox/internal/store"
)

func adminPasswordFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	raw := []byte(`{"auth_token":"synthetic-config-secret","data_dir":"data","timezone":"UTC","accounts":[]}`)
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "data", "state.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, u := range []store.User{
		{Name: "boxadmin", Role: store.RoleAdmin, PassHash: password.Hash("synthetic-old-password")},
		{Name: "alice", Role: store.RoleUser, PassHash: "synthetic-other-hash"},
	} {
		if err := s.CreateUser(u); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateToken(u.Name+"-token", u.Name); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateToken("second-admin-token", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(store.Session{ID: "fixture", Name: "Keep project", User: "boxadmin", Agent: "claude", Status: store.StatusStopped, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetQuotaEnforced("boxadmin", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant("boxadmin", 1000000, "synthetic-grant", "fixture", "boxadmin"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsage(store.UsageEvent{User: "boxadmin", SessionID: "fixture", TurnID: "synthetic-turn", Agent: "claude", Kind: store.UsageKindChat, CostMicroUSD: 42, TS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return configPath, dbPath
}

func TestAdminPasswordRecoveryPreservesInstance(t *testing.T) {
	configPath, dbPath := adminPasswordFixture(t)
	s, err := store.OpenForMaintenance(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	admin, _ := s.GetUser("boxadmin")
	sessions := s.All()
	quota, _ := s.GetQuota("boxadmin")
	ledger := s.ListLedger(store.LedgerFilter{})
	usage := s.ListUsage(store.UsageFilter{})
	configBefore, _ := os.ReadFile(configPath)
	var out, diagnostics bytes.Buffer
	secret := []byte("synthetic-new-password")
	err = resetAdminPassword(t.Context(), []string{"--config", configPath, "--user", "boxadmin"}, &out, &diagnostics, func(context.Context) ([]byte, error) {
		lock, err := os.OpenFile(filepath.Join(filepath.Dir(dbPath), "agentbox.lock"), os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			t.Fatal("password prompt did not hold the instance lock")
		}
		return secret, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, ok := s.GetUser("boxadmin")
	if !ok || !password.Verify(after.PassHash, "synthetic-new-password") || password.Verify(after.PassHash, "synthetic-old-password") || after.Role != admin.Role || !after.CreatedAt.Equal(admin.CreatedAt) {
		t.Fatal("password/identity recovery failed")
	}
	for _, token := range []string{"boxadmin-token", "second-admin-token"} {
		if _, ok := s.TokenUser(token); ok {
			t.Fatal("administrator token survived")
		}
	}
	if u, ok := s.TokenUser("alice-token"); !ok || u.PassHash != "synthetic-other-hash" {
		t.Fatal("other user was modified")
	}
	afterQuota, _ := s.GetQuota("boxadmin")
	configAfter, _ := os.ReadFile(configPath)
	if !reflect.DeepEqual(s.All(), sessions) || !reflect.DeepEqual(afterQuota, quota) || !reflect.DeepEqual(s.ListLedger(store.LedgerFilter{}), ledger) || !reflect.DeepEqual(s.ListUsage(store.UsageFilter{}), usage) || !bytes.Equal(configBefore, configAfter) {
		t.Fatal("recovery changed instance data")
	}
	for _, b := range secret {
		if b != 0 {
			t.Fatal("password byte buffer not cleared")
		}
	}
	if strings.Contains(out.String()+diagnostics.String(), "synthetic-new-password") || strings.Contains(out.String(), after.PassHash) {
		t.Fatal("secret in output")
	}
	var report struct {
		PasswordReset bool `json:"password_reset"`
		TokensRevoked bool `json:"tokens_revoked"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || !report.PasswordReset || !report.TokensRevoked {
		t.Fatal("missing completion report", err)
	}
}

func TestAdminPasswordRecoveryRefusalsAndRollback(t *testing.T) {
	for _, failure := range []string{"locked", "not-admin", "missing-user", "short", "read-error", "cancelled", "config-changed", "revoke-failed"} {
		t.Run(failure, func(t *testing.T) {
			configPath, dbPath := adminPasswordFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			user := "boxadmin"
			if failure == "not-admin" {
				user = "alice"
			}
			if failure == "missing-user" {
				user = "missing"
			}
			if failure == "locked" {
				lock, err := os.OpenFile(filepath.Join(filepath.Dir(dbPath), "agentbox.lock"), os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "revoke-failed" {
				db, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(`CREATE TRIGGER reject_revocation BEFORE DELETE ON tokens BEGIN SELECT RAISE(ABORT, 'synthetic revocation failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			var out bytes.Buffer
			err := resetAdminPassword(ctx, []string{"--config", configPath, "--user", user}, &out, io.Discard, func(context.Context) ([]byte, error) {
				called = true
				switch failure {
				case "short":
					return []byte("short"), nil
				case "read-error":
					return nil, errors.New("synthetic read failure")
				case "cancelled":
					cancel()
				case "config-changed":
					if err := os.WriteFile(configPath, []byte(`{"auth_token":"synthetic-secret","data_dir":"other","timezone":"UTC"}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return []byte("synthetic-new-password"), nil
			})
			if err == nil || out.Len() != 0 {
				t.Fatal("reported success after refusal")
			}
			wantPrompt := failure != "locked" && failure != "not-admin" && failure != "missing-user"
			if called != wantPrompt {
				t.Fatalf("failure %s reached prompt=%v, want %v; error: %v", failure, called, wantPrompt, err)
			}
			wantError := map[string]string{
				"locked": "cannot lock data directory", "not-admin": "existing administrator", "missing-user": "existing administrator",
				"short": "password must be", "read-error": "synthetic read failure", "cancelled": "context canceled",
				"config-changed": "data_dir changed", "revoke-failed": "synthetic revocation failure",
			}[failure]
			if !strings.Contains(err.Error(), wantError) {
				t.Fatalf("wrong refusal for %s: %v", failure, err)
			}
			s, err := store.OpenForMaintenance(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			admin, _ := s.GetUser("boxadmin")
			if !password.Verify(admin.PassHash, "synthetic-old-password") {
				t.Fatal("failure changed password")
			}
			for _, token := range []string{"boxadmin-token", "second-admin-token", "alice-token"} {
				if _, ok := s.TokenUser(token); !ok {
					t.Fatal("failure revoked token")
				}
			}
		})
	}
}

func TestAdminPasswordRequiresExplicitTarget(t *testing.T) {
	for _, args := range [][]string{nil, {"--config", "config.json"}, {"--user", "boxadmin"}, {"--password", "do-not-accept"}} {
		if err := resetAdminPassword(t.Context(), args, io.Discard, io.Discard, func(context.Context) ([]byte, error) { t.Fatal("prompted without target"); return nil, nil }); err == nil {
			t.Fatal("accepted ambiguous target/password argument")
		}
	}
}

// Run the real main/maintenance dispatch in an isolated test subprocess.
func TestAdminPasswordTerminalHelper(t *testing.T) {
	if os.Getenv("AGENTBOX_ADMIN_PASSWORD_TEST_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"agentbox"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestAdminPasswordTerminal(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for real pseudoterminal acceptance")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"success", "mismatch", "sigint", "sigterm", "no-tty"} {
		t.Run(scenario, func(t *testing.T) {
			configPath, dbPath := adminPasswordFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, python, "testdata/admin_password_terminal.py", binary, configPath, scenario)
			cmd.Env = append(os.Environ(), "AGENTBOX_ADMIN_PASSWORD_TEST_HELPER=1")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("terminal acceptance: %v\n%s", err, output)
			}
			s, err := store.OpenForMaintenance(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			u, _ := s.GetUser("boxadmin")
			want := "synthetic-old-password"
			if scenario == "success" {
				want = "synthetic-terminal-password"
			}
			if !password.Verify(u.PassHash, want) {
				t.Fatal("unexpected persisted password")
			}
			_, tokenSurvived := s.TokenUser("boxadmin-token")
			if tokenSurvived != (scenario != "success") {
				t.Fatal("unexpected token revocation")
			}
		})
	}
}
