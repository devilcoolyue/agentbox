package mcpconfig

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Service, string, Apply) {
	t.Helper()
	root := t.TempDir()
	s := New(root)
	home := filepath.Join(root, "users/alice/sessions/s1/home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(native, []byte(`{"hasCompletedOnboarding":true,"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	apply := func(_ context.Context, name string, expected, desired *Definition) error {
		n, _, err := s.native("alice", "s1")
		if err != nil {
			return err
		}
		if !same(n[name], expected) {
			return ErrConflict
		}
		if desired == nil {
			delete(n, name)
		} else {
			n[name] = desired
		}
		raw, _ := json.Marshal(map[string]any{"hasCompletedOnboarding": true, "mcpServers": n})
		return os.WriteFile(native, raw, 0600)
	}
	return s, native, apply
}
func entry(command string) Entry {
	return Entry{Config: Definition{Type: "stdio", Command: command, Env: map[string]string{"TOKEN": "synthetic-secret"}}}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func TestInheritanceUpdateDeleteAndSecrets(t *testing.T) {
	s, native, apply := fixture(t)
	e := entry("first")
	must(t, s.Put("alice", "", "demo", 0, &e))
	must(t, s.Sync(t.Context(), "alice", "s1", apply))
	v, err := s.View("alice", "s1")
	must(t, err)
	if v.Items[0].Status != "applied" || v.Items[0].Config.Env["TOKEN"] != KeepSecret {
		t.Fatalf("bad view: %+v", v)
	}
	actual, err := s.Definition("alice", "s1", "demo")
	must(t, err)
	if actual.Env["TOKEN"] != "synthetic-secret" {
		t.Fatal("mask mutated secret")
	}
	override := entry("override")
	override.Config.Env["TOKEN"] = KeepSecret
	must(t, s.Put("alice", "s1", "demo", 0, &override))
	must(t, s.Sync(t.Context(), "alice", "s1", apply))
	must(t, s.Put("alice", "s1", "demo", 1, nil))
	must(t, s.Sync(t.Context(), "alice", "s1", apply))
	n, _, err := s.native("alice", "s1")
	must(t, err)
	if n["demo"].Command != "first" {
		t.Fatal("inheritance not restored")
	}
	updated := entry("second")
	updated.Config.Env["TOKEN"] = KeepSecret
	must(t, s.Put("alice", "", "demo", 1, &updated))
	must(t, s.Sync(t.Context(), "alice", "s1", apply))
	disabled := Entry{Disabled: true}
	must(t, s.Put("alice", "s1", "demo", 2, &disabled))
	must(t, s.Sync(t.Context(), "alice", "s1", apply))
	n, _, _ = s.native("alice", "s1")
	if n["demo"] != nil {
		t.Fatal("disable did not remove managed server")
	}
	must(t, s.Put("alice", "s1", "demo", 3, nil))
	must(t, s.Sync(t.Context(), "alice", "s1", apply))
	must(t, s.Put("alice", "", "demo", 2, nil))
	must(t, s.Sync(t.Context(), "alice", "s1", apply))
	raw, _ := os.ReadFile(native)
	if !strings.Contains(string(raw), "hasCompletedOnboarding") || strings.Contains(string(raw), "demo") {
		t.Fatal(string(raw))
	}
}
func TestExplicitAdoptionAndRelease(t *testing.T) {
	s, native, apply := fixture(t)
	e := entry("managed")
	must(t, s.Put("alice", "", "demo", 0, &e))
	must(t, os.WriteFile(native, []byte(`{"mcpServers":{"demo":{"command":"native"},"other":{"command":"untouched"}}}`), 0600))
	if err := s.Sync(t.Context(), "alice", "s1", apply); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrote native: %v", err)
	}
	v, err := s.View("alice", "s1")
	must(t, err)
	if v.Items[0].Status != "conflict" {
		t.Fatal(v)
	}
	if !errors.Is(s.Adopt("alice", "s1", "demo", 0, "stale", false), ErrRevision) {
		t.Fatal("stale adoption accepted")
	}
	must(t, s.Adopt("alice", "s1", "demo", 0, v.Items[0].NativeRevision, false))
	must(t, s.Sync(t.Context(), "alice", "s1", apply))
	must(t, os.WriteFile(native, []byte(`{"mcpServers":{"demo":{"command":"edited"},"other":{"command":"untouched"}}}`), 0600))
	if !errors.Is(s.Sync(t.Context(), "alice", "s1", apply), ErrConflict) {
		t.Fatal("external edit lost")
	}
	v, err = s.View("alice", "s1")
	must(t, err)
	must(t, s.Adopt("alice", "s1", "demo", 1, v.Items[0].NativeRevision, true))
	must(t, s.Sync(t.Context(), "alice", "s1", apply))
	n, _, _ := s.native("alice", "s1")
	if n["demo"].Command != "edited" || n["other"].Command != "untouched" {
		t.Fatal(n)
	}
	v, err = s.View("alice", "s1")
	must(t, err)
	if v.Items[0].Status == "conflict" {
		t.Fatal("release still conflicts")
	}
}
func TestRecoverInterruptedNativeWrite(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "after-remove", true: "after-add"}[after], func(t *testing.T) {
			s, native, apply := fixture(t)
			e := entry("old")
			must(t, s.Put("alice", "", "demo", 0, &e))
			must(t, s.Sync(t.Context(), "alice", "s1", apply))
			e = entry("new")
			must(t, s.Put("alice", "", "demo", 1, &e))
			err := s.Sync(t.Context(), "alice", "s1", func(ctx context.Context, n string, a, b *Definition) error {
				if after {
					must(t, apply(ctx, n, a, b))
				} else {
					must(t, os.WriteFile(native, []byte(`{"mcpServers":{}}`), 0600))
				}
				return errors.New("lost response")
			})
			if err == nil {
				t.Fatal("missing failure")
			}
			// A new service simulates a process restart with only durable state.
			must(t, New(s.data).Sync(t.Context(), "alice", "s1", apply))
			n, _, _ := s.native("alice", "s1")
			if n["demo"].Command != "new" {
				t.Fatal(n)
			}
		})
	}
}
func TestImportRevisionIsolationAndValidation(t *testing.T) {
	s, _, _ := fixture(t)
	e := entry("python3")
	must(t, s.Put("alice", "", "demo", 0, &e))
	if !errors.Is(s.Put("alice", "", "demo", 0, &e), ErrRevision) {
		t.Fatal("stale write accepted")
	}
	view, err := s.View("bob", "")
	must(t, err)
	if len(view.Items) != 0 {
		t.Fatal("cross-user read")
	}
	must(t, s.Import("alice", "", 1, map[string]Definition{"one": {Command: "node"}}))
	if err = s.Import("alice", "", 2, map[string]Definition{"two": {Command: "node"}, "../escape": {Command: "node"}}); err == nil {
		t.Fatal("unsafe name")
	}
	view, err = s.View("alice", "")
	must(t, err)
	if len(view.Items) != 2 {
		t.Fatal("partial import")
	}
	if err = s.Put("../escape", "", "demo", 0, &e); err == nil {
		t.Fatal("unsafe owner")
	}
	e.Config.Env = map[string]string{"TOKEN": KeepSecret}
	must(t, s.Put("alice", "", "demo", 2, &e))
	e.Config.Env = map[string]string{}
	must(t, s.Put("alice", "", "demo", 3, &e))
	d, err := s.Definition("alice", "s1", "demo")
	must(t, err)
	if len(d.Env) != 0 {
		t.Fatal("secret not cleared")
	}
	for _, d := range []Definition{{Type: "http", URL: "file:///etc/passwd"}, {Type: "http", URL: "https://user:secret@a.test/"}, {Type: "stdio", Command: "x", Env: map[string]string{"bad key": "x"}}, {Type: "http", URL: "https://a.test/mcp", Headers: map[string]string{"Auth": "x\r\ny"}}} {
		if Validate("demo", d) == nil {
			t.Fatalf("accepted %+v", d)
		}
	}
}
func TestSymlinkAndCancelledLock(t *testing.T) {
	s, native, _ := fixture(t)
	outside := filepath.Join(t.TempDir(), "secret")
	must(t, os.WriteFile(outside, []byte(`{"mcpServers":{"secret":{"command":"secret"}}}`), 0600))
	must(t, os.Remove(native))
	must(t, os.Symlink(outside, native))
	if _, err := s.View("alice", "s1"); err == nil {
		t.Fatal("followed native symlink")
	}
	e := entry("x")
	must(t, s.Put("alice", "", "demo", 0, &e))
	path := filepath.Join(s.data, "users/alice/mcp.json")
	must(t, os.Remove(path))
	must(t, os.Symlink(outside, path))
	if err := s.Put("alice", "", "demo", 0, &e); err == nil {
		t.Fatal("followed canonical symlink")
	}
	unlock, _ := s.lock(context.Background(), "bob")
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if !errors.Is(s.Sync(ctx, "bob", "s1", nil), context.DeadlineExceeded) {
		t.Fatal("lock not cancellable")
	}
}

func TestExternalSourcesAndUnsupportedNativeStayReadOnly(t *testing.T) {
	s, native, _ := fixture(t)
	must(t, os.WriteFile(native, []byte(`{"projects":{"/workspace":{"mcpServers":{"local-server":{"command":"node","env":{"TOKEN":"local-secret"}}}}},"mcpServers":{"oauth":{"type":"http","url":"https://example.com/mcp","oauth":{"clientId":"sensitive"}}}}`), 0600))
	plugins := filepath.Join(filepath.Dir(native), ".claude/plugins")
	must(t, os.MkdirAll(plugins, 0700))
	must(t, os.WriteFile(filepath.Join(plugins, "installed_plugins.json"), []byte(`{"plugins":{"demo@market":[{"installPath":"/outside/do-not-follow"}]}}`), 0600))
	v, err := s.View("alice", "s1")
	must(t, err)
	if len(v.External) != 2 || v.Items[0].Config.Type != "unsupported" {
		t.Fatalf("bad external view: %+v", v)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "local-secret") || strings.Contains(string(raw), "sensitive") || strings.Contains(string(raw), "outside") {
		t.Fatal("external metadata leaked payload")
	}
	if s.Adopt("alice", "s1", "oauth", 0, v.Items[0].NativeRevision, false) == nil {
		t.Fatal("lossy adoption accepted")
	}
}
