package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentbox/internal/store"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	data := filepath.Join(base, "state")
	st, err := store.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) // leave WAL active while taking online snapshot
	if err := st.CreateUser(store.User{Name: "alice", Role: store.RoleUser, PassHash: "fake-hash"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(store.Session{ID: "s1", User: "alice", Agent: "claude", Name: "project"}); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external credentials")
	files := map[string]string{
		filepath.Join(data, "creds", "pool", "auth.json"):                                               "pool-secret",
		filepath.Join(data, "home-template", ".bashrc"):                                                 "global-template",
		filepath.Join(data, "users", "alice", "home-template", ".claude", "skills", "demo", "SKILL.md"): "user-template",
		filepath.Join(data, "users", "alice", "sessions", "s1", "workspace", "run.sh"):                  "#!/bin/sh\necho preserved\n",
		filepath.Join(data, "users", "alice", "sessions", "s1", "home", ".claude", ".credentials.json"): "session-secret",
		filepath.Join(data, "users", "alice", "sessions", "s1", "chats", "t1.jsonl"):                    "{\"kind\":\"user\",\"text\":\"hello\"}\n",
		filepath.Join(data, "users", "alice", "shared", "file.txt"):                                     "shared-data",
		filepath.Join(base, "accounts", "legacy", "auth.json"):                                          "legacy-secret",
		filepath.Join(external, "auth.json"):                                                            "external-secret",
	}
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(data, "users/alice/sessions/s1/workspace/run.sh")
	if err := os.Chmod(script, 0o750); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1700000000, 123456000)
	if err := os.Chtimes(script, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/shared", filepath.Join(data, "home-template", "shared-link")); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"data_dir": "state", "auth_token": "test-only-no-real-secret", "unknown_future_field": map[string]any{"keep": true},
		"accounts": []map[string]string{{"id": "pool", "type": "claude", "credentials_dir": "state/creds/pool"}, {"id": "external", "type": "codex", "credentials_dir": external}, {"id": "same", "type": "codex", "credentials_dir": external}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(base, "config.json")
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, data
}

func TestFullBackupRestoreRoundTrip(t *testing.T) {
	cfg, data := fixture(t)
	archive := filepath.Join(t.TempDir(), "full.tar.gz")
	checks := 0
	m, err := Create(t.Context(), Options{Config: cfg, Output: archive, Full: true, CheckStopped: func(context.Context, []string) error { checks++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if checks != 2 || m.Mode != "full" {
		t.Fatalf("checks=%d mode=%s", checks, m.Mode)
	}
	if m.Credentials["external"] != m.Credentials["same"] {
		t.Fatal("shared account root duplicated")
	}
	if _, err = Verify(t.Context(), archive); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err = Restore(t.Context(), archive, target); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(target, "data/state.db"+suffix)); !os.IsNotExist(err) {
			t.Fatalf("restore retained SQLite sidecar %s: %v", suffix, err)
		}
	}
	for _, rel := range []string{"creds/pool/auth.json", "home-template/.bashrc", "users/alice/sessions/s1/workspace/run.sh", "users/alice/sessions/s1/home/.claude/.credentials.json", "users/alice/sessions/s1/chats/t1.jsonl", "users/alice/shared/file.txt", "users/alice/home-template/.claude/skills/demo/SKILL.md"} {
		want, _ := os.ReadFile(filepath.Join(data, rel))
		got, err := os.ReadFile(filepath.Join(target, "data", rel))
		if err != nil || !bytes.Equal(want, got) {
			t.Fatalf("%s: %q %v", rel, got, err)
		}
	}
	before, _ := os.Stat(filepath.Join(data, "users/alice/sessions/s1/workspace/run.sh"))
	after, _ := os.Stat(filepath.Join(target, "data/users/alice/sessions/s1/workspace/run.sh"))
	if after.Mode().Perm() != before.Mode().Perm() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("file metadata changed: %v %v", before, after)
	}
	if link, err := os.Readlink(filepath.Join(target, "data/home-template/shared-link")); err != nil || link != "/shared" {
		t.Fatalf("link: %q %v", link, err)
	}
	raw, _ := os.ReadFile(filepath.Join(target, "config.json"))
	var restored map[string]json.RawMessage
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if string(restored["data_dir"]) != `"data"` || !bytes.Contains(restored["unknown_future_field"], []byte("true")) {
		t.Fatalf("config fields lost: %s", raw)
	}
	var accounts []map[string]string
	_ = json.Unmarshal(restored["accounts"], &accounts)
	external := filepath.Join(target, accounts[1]["credentials_dir"], "auth.json")
	if got, err := os.ReadFile(external); err != nil || string(got) != "external-secret" {
		t.Fatalf("external credentials missing: %q %v", got, err)
	}
	db, err := openDB(filepath.Join(target, "data/state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var user string
	if err = db.QueryRow("SELECT name FROM users").Scan(&user); err != nil || user != "alice" {
		t.Fatalf("WAL data missing: %s %v", user, err)
	}
	if _, err = Restore(t.Context(), archive, target); err == nil {
		t.Fatal("overwrote existing instance")
	}
}

func TestSystemBackupIncludesTemplatesAndCredentialsOnly(t *testing.T) {
	cfg, _ := fixture(t)
	archive := filepath.Join(t.TempDir(), "system.tar.gz")
	m, err := Create(t.Context(), Options{Config: cfg, Output: archive})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Entries {
		if strings.Contains(e.Name, "sessions/") || strings.Contains(e.Name, "shared/file") {
			t.Fatalf("system backup included workspace: %s", e.Name)
		}
	}
	target := filepath.Join(t.TempDir(), "system")
	if _, err = Restore(t.Context(), archive, target); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"data/home-template/.bashrc", "data/users/alice/home-template/.claude/skills/demo/SKILL.md", "data/creds/pool/auth.json", "accounts/legacy/auth.json", "credentials/1/auth.json"} {
		if _, err = os.Stat(filepath.Join(target, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}

func TestFullBackupRequiresStoppedWriters(t *testing.T) {
	cfg, data := fixture(t)
	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	opts := Options{Config: cfg, Output: out, Full: true}
	if _, err := Create(t.Context(), opts); err == nil {
		t.Fatal("missing Docker check accepted")
	}
	denied := errors.New("container still running")
	opts.CheckStopped = func(context.Context, []string) error { return denied }
	if _, err := Create(t.Context(), opts); !errors.Is(err, denied) {
		t.Fatalf("container check: %v", err)
	}
	lock, err := os.OpenFile(filepath.Join(data, "agentbox.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	opts.CheckStopped = func(context.Context, []string) error { t.Fatal("checked Docker before lock"); return nil }
	if _, err = Create(t.Context(), opts); err == nil {
		t.Fatal("running service accepted")
	}
	if _, err = os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("published incomplete backup")
	}
}

func rewriteArchive(t *testing.T, source string, edit func(*tar.Header, []byte) (*tar.Header, []byte)) string {
	t.Helper()
	f, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	dest := filepath.Join(t.TempDir(), "modified.tar.gz")
	out, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := gzip.NewWriter(out)
	tw := tar.NewWriter(zw)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		hdr, raw = edit(hdr, raw)
		hdr.Size = int64(len(raw))
		if err = tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestRestoreRejectsCorruptionAndUnsafeArchivesBeforePublication(t *testing.T) {
	cfg, _ := fixture(t)
	archive := filepath.Join(t.TempDir(), "system.tar.gz")
	if _, err := Create(t.Context(), Options{Config: cfg, Output: archive}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"corruption", "traversal", "symlink-parent", "unknown-format"} {
		t.Run(name, func(t *testing.T) {
			edited := rewriteArchive(t, archive, func(h *tar.Header, b []byte) (*tar.Header, []byte) {
				switch name {
				case "corruption":
					if h.Name == "config.json" {
						b = append(b, ' ')
					}
				case "traversal":
					if h.Name == "config.json" {
						h.Name = "../escape"
					}
				case "symlink-parent":
					if h.Name == "data/creds" {
						h.Typeflag = tar.TypeSymlink
						h.Linkname = "/tmp"
					}
				case "unknown-format":
					if h.Name == "manifest.json" {
						var m Manifest
						_ = json.Unmarshal(b, &m)
						m.Version = 999
						b, _ = json.Marshal(m)
					}
				}
				return h, b
			})
			target := filepath.Join(t.TempDir(), "new")
			if _, err := Restore(t.Context(), edited, target); err == nil {
				t.Fatal("invalid backup accepted")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatal("invalid backup published target")
			}
		})
	}
	raw, _ := os.ReadFile(archive)
	truncated := filepath.Join(t.TempDir(), "truncated.tar.gz")
	_ = os.WriteFile(truncated, raw[:len(raw)-4], 0o600)
	if _, err := Verify(t.Context(), truncated); err == nil {
		t.Fatal("missing gzip trailer accepted")
	}
}

func TestMissingConfiguredCredentialsFailsBackup(t *testing.T) {
	cfg, data := fixture(t)
	if err := os.RemoveAll(filepath.Join(data, "creds/pool")); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Create(t.Context(), Options{Config: cfg, Output: output}); err == nil {
		t.Fatal("silently omitted configured credentials")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("published backup with missing credentials")
	}
}

// Confirm the backup helper never invokes migrations against the source.
func TestBackupSnapshotDoesNotMigrateSource(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "state.db")
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE old_table(value TEXT); INSERT INTO old_table VALUES('keep')"); err != nil {
		t.Fatal(err)
	}
	if err = snapshotDB(t.Context(), src, filepath.Join(base, "copy.db")); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("source schema changed: %d %v", n, err)
	}
}

func TestMCPSystemBackupRestore(t *testing.T) {
	cfg, data := fixture(t)
	paths := []string{"users/alice/mcp.json", "users/alice/sessions/s1/mcp.json"}
	for _, rel := range paths {
		if err := os.WriteFile(filepath.Join(data, rel), []byte(`{"version":1,"entries":{},"revision":4}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "mcp.tar.gz")
	if _, err := Create(t.Context(), Options{Config: cfg, Output: archive}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(t.Context(), archive, target); err != nil {
		t.Fatal(err)
	}
	for _, rel := range paths {
		raw, err := os.ReadFile(filepath.Join(target, "data", rel))
		if err != nil || !strings.Contains(string(raw), `"revision":4`) {
			t.Fatalf("MCP state missing: %s %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "data/users/alice/sessions/s1/home/.claude/.credentials.json")); !os.IsNotExist(err) {
		t.Fatal("system backup included runtime home")
	}
}
