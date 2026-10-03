package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agentbox/internal/store"
	"agentbox/internal/syncclient"
	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

type engineFixture struct {
	engine        *syncclient.Engine
	saved         syncclient.SavedBinding
	local, remote string
	stateDir      string
	server        *Server
	session       store.Session
	project       store.ClientProject
}

// Only this test peer advertises sync=1. The production capability stays zero.
func newEngineFixture(t *testing.T, enabled bool, wrap func(http.Handler) http.Handler) *engineFixture {
	t.Helper()
	s, sess, handler := clientTestServer(t)
	project, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Root", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	upstream := handler
	handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if enabled && r.URL.Path == "/api/clients/capabilities" {
			response := httptest.NewRecorder()
			upstream.ServeHTTP(response, r)
			var caps syncproto.ServerIdentity
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &caps) != nil {
				t.Error("fixture capabilities failed")
				w.WriteHeader(500)
				return
			}
			caps.Features["sync"] = 1
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(caps)
			return
		}
		upstream.ServeHTTP(w, r)
	})
	if wrap != nil {
		handler = wrap(handler)
	}
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	remote, err := syncclient.NewRemote(httpServer.URL, "client-fixture-alice", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	identity, err := remote.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, err := syncclient.OpenState(filepath.Join(private, "sync"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	root, err := syncfs.Open(local)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := root.Probe()
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	base, _ := syncproto.NormalizeServer(httpServer.URL)
	saved, err := state.Register(syncproto.Binding{Version: 1, Server: base, ServerID: identity.ServerID, User: "alice", Workspace: sess.ID, Project: project.ID, ProjectPath: ".", LocalID: caps.DirectoryID}, local)
	if err != nil {
		t.Fatal(err)
	}
	return &engineFixture{engine: &syncclient.Engine{State: state, Remote: remote}, stateDir: filepath.Join(private, "sync"), saved: saved, local: local, remote: s.workspaceDir(sess), server: s, session: sess, project: project}
}
func writeEngineFile(t *testing.T, root, name, content string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
func engineRound(t *testing.T, f *engineFixture) syncclient.SavedBinding {
	t.Helper()
	preview, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Pending != nil || saved.Baseline == nil {
		t.Fatal("batch did not commit")
	}
	return saved
}
func TestSyncEngineBidirectionalBatchDirectoriesAndBaseline(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	writeEngineFile(t, f.local, "local/sub/中文.txt", "local\r\n\x00")
	writeEngineFile(t, f.remote, "remote/sub/run", "remote\r\n")
	first := engineRound(t, f)
	for _, pair := range []struct{ root, path, want string }{{f.remote, "local/sub/中文.txt", "local\r\n\x00"}, {f.local, "remote/sub/run", "remote\r\n"}} {
		raw, err := os.ReadFile(filepath.Join(pair.root, filepath.FromSlash(pair.path)))
		if err != nil || string(raw) != pair.want {
			t.Fatal("transfer changed raw bytes", err)
		}
	}
	if err := os.RemoveAll(filepath.Join(f.remote, "remote")); err != nil {
		t.Fatal(err)
	}
	writeEngineFile(t, f.local, "local/sub/中文.txt", "edited")
	second := engineRound(t, f)
	if second.Revision <= first.Revision {
		t.Fatal("baseline did not advance")
	}
	if _, err := os.Stat(filepath.Join(f.local, "remote")); !os.IsNotExist(err) {
		t.Fatal("directory remained after deletion", err)
	}
	matches, err := filepath.Glob(filepath.Join(f.local, ".agentbox-sync", "recovery", "*.bak"))
	if err != nil || len(matches) != 1 {
		t.Fatal("missing local recovery", matches, err)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil || string(raw) != "remote\r\n" {
		t.Fatal("local recovery lost", err)
	}
	// A deletion on the other side must use remote conditional delete and rmdir.
	if err = os.RemoveAll(filepath.Join(f.local, "local")); err != nil {
		t.Fatal(err)
	}
	engineRound(t, f)
	if _, err = os.Stat(filepath.Join(f.remote, "local")); !os.IsNotExist(err) {
		t.Fatal("remote directories remained", err)
	}
	p, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil || len(p.Plan.Operations) != 0 || len(p.Plan.Conflicts) != 0 {
		t.Fatal("committed baseline not converged", p, err)
	}
	if f.server.syncLeases().Active(f.session.ID, f.project.ID) {
		t.Fatal("completed engine leaked lease")
	}
}
func TestSyncEngineRefusesDisabledStaleAndConflictWithoutWriting(t *testing.T) {
	for _, kind := range []string{"disabled", "stale", "confirmation", "conflict", "rules"} {
		t.Run(kind, func(t *testing.T) {
			f := newEngineFixture(t, kind != "disabled", nil)
			writeEngineFile(t, f.local, "file", "local")
			if kind == "conflict" {
				writeEngineFile(t, f.remote, "file", "remote")
			}
			p, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
			if err != nil {
				t.Fatal(err)
			}
			confirm := p.Plan.Digest
			switch kind {
			case "stale":
				writeEngineFile(t, f.local, "file", "later")
			case "confirmation":
				confirm = ""
			case "rules":
				writeEngineFile(t, f.remote, ".agentboxignore", "file\n")
			}
			_, err = f.engine.Apply(t.Context(), f.saved.ID, p, confirm)
			if err == nil {
				t.Fatal("unsafe apply accepted")
			}
			if kind == "disabled" && !errors.Is(err, syncclient.ErrSyncUnavailable) {
				t.Fatal(err)
			}
			saved, err := f.engine.State.Load(f.saved.ID)
			if err != nil || saved.Pending != nil || saved.Baseline != nil {
				t.Fatal("refused apply changed persistent state", err)
			}
			if kind != "conflict" {
				if _, err = os.Stat(filepath.Join(f.remote, "file")); !os.IsNotExist(err) {
					t.Fatal("refused apply wrote remote file", err)
				}
			}
		})
	}
}
func TestSyncEngineLostResponseRetainsOperationAndDoesNotReplay(t *testing.T) {
	var calls atomic.Int32
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/sync/apply") && calls.Add(1) == 2 {
				response := httptest.NewRecorder()
				next.ServeHTTP(response, r)
				if response.Code != 200 {
					t.Error("fixture publication failed", response.Code, response.Body.String())
				}
				w.Header().Set("X-Agentbox-Server-ID", response.Header().Get("X-Agentbox-Server-ID"))
				w.WriteHeader(503)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	writeEngineFile(t, f.local, "a", "first")
	writeEngineFile(t, f.local, "b", "second")
	p, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, p, p.Plan.Digest); err == nil {
		t.Fatal("lost response accepted")
	}
	saved, err := f.engine.State.Load(f.saved.ID)
	if err != nil || saved.Pending == nil || saved.Baseline != nil {
		t.Fatal("partial batch baseline committed", err)
	}
	if saved.Pending.Operations[0].Status != "verified" || saved.Pending.Operations[1].Status != "started" {
		t.Fatal("partial progress lost", saved.Pending.Operations)
	}
	status, err := f.engine.Remote.Operation(t.Context(), f.session.ID, saved.Pending.Operations[1].ID)
	if err != nil || status.Operation.Status != "applied" {
		t.Fatal("cannot reconcile lost response", status, err)
	}
	before := calls.Load()
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, p, p.Plan.Digest); !errors.Is(err, syncclient.ErrPending) {
		t.Fatal("blind replay permitted", err)
	}
	if calls.Load() != before {
		t.Fatal("retry sent another mutation")
	}
}
func TestSyncEngineLeaseLostBeforeLocalPublicationKeepsOriginal(t *testing.T) {
	var failRenew atomic.Bool
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/sync/lease") && failRenew.Load() {
				w.WriteHeader(409)
				return
			}
			next.ServeHTTP(w, r)
			if strings.HasSuffix(r.URL.Path, "/sync/file") {
				failRenew.Store(true)
			}
		})
	})
	writeEngineFile(t, f.local, "file", "old")
	writeEngineFile(t, f.remote, "file", "old")
	base := engineRound(t, f)
	writeEngineFile(t, f.remote, "file", "new")
	p, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, p, p.Plan.Digest); err == nil {
		t.Fatal("lost lease allowed local write")
	}
	saved, err := f.engine.State.Load(f.saved.ID)
	if err != nil || saved.Pending == nil || saved.Baseline.Local.Entries["file"] != base.Baseline.Local.Entries["file"] {
		t.Fatal("failed publication advanced baseline", err)
	}
	if raw, err := os.ReadFile(filepath.Join(f.local, "file")); err != nil || string(raw) != "old" {
		t.Fatal("lease failure overwrote local", err)
	}
	op := saved.Pending.Operations[0]
	if op.Status != "started" || op.Recovery == nil {
		t.Fatal("pre-publication recovery was not journaled", op)
	}
	if raw, err := os.ReadFile(filepath.Join(f.local, filepath.FromSlash(op.Recovery.Path))); err != nil || string(raw) != "old" {
		t.Fatal("recovery not available", err)
	}
}
func TestSyncEngineCanceledBeforeBeginLeavesNoPending(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	writeEngineFile(t, f.local, "file", "local")
	p, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = f.engine.Apply(ctx, f.saved.ID, p, p.Plan.Digest); err == nil {
		t.Fatal("canceled work ran")
	}
	saved, err := f.engine.State.Load(f.saved.ID)
	if err != nil || saved.Pending != nil {
		t.Fatal("canceled work created pending", err)
	}
}

func TestSyncEngineRulesEditedDuringDownloadRefusesLocalPublication(t *testing.T) {
	var local string
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/sync/file") {
				if err := os.WriteFile(filepath.Join(local, ".agentboxignore"), []byte("file\n"), 0644); err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	local = f.local
	writeEngineFile(t, f.local, "file", "old")
	writeEngineFile(t, f.remote, "file", "old")
	engineRound(t, f)
	writeEngineFile(t, f.remote, "file", "new")
	preview, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest); !errors.Is(err, syncclient.ErrRulesChanged) {
		t.Fatal("ignored file publication allowed", err)
	}
	raw, err := os.ReadFile(filepath.Join(local, "file"))
	if err != nil || string(raw) != "old" {
		t.Fatal("newly ignored file overwritten", err)
	}
}
