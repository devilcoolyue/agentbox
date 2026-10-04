package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"agentbox/internal/syncclient"
	"agentbox/internal/syncproto"
)

func mutationEntry(text string) *syncproto.Entry {
	return &syncproto.Entry{Kind: "file", Hash: syncproto.HashBytes([]byte(text)), Size: int64(len(text))}
}
func mutationFixture(t *testing.T) (*Server, store.Session, http.Handler, syncproto.Lease, syncproto.Mutation) {
	t.Helper()
	s, sess, handler := clientTestServer(t)
	p, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Root", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.syncLeases().Acquire(sess.ID, p.ID, p.Path, "fixture-device")
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := syncproto.ParseRules("")
	operation := syncproto.Mutation{Version: 1, ID: strings.Repeat("a", 32), Project: p.ID, Revision: p.Revision, RulesHash: rules.Hash(), Device: lease.Device, Generation: lease.Generation, Path: "file", Kind: "replace", After: mutationEntry("new\r\n\x00")}
	return s, sess, handler, lease, operation
}
func mutationRequest(handler http.Handler, operation syncproto.Mutation, lease syncproto.Lease, source io.Reader, user string) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(operation)
	r := httptest.NewRequest("POST", "/api/sessions/s1/sync/apply", source)
	if user != "" {
		r.Header.Set("Authorization", "Bearer client-fixture-"+user)
	}
	r.Header.Set("X-Agentbox-Sync-Lease", lease.Token)
	r.Header.Set("X-Agentbox-Sync-Request", base64.RawURLEncoding.EncodeToString(raw))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}
func TestSyncMutationHTTPCreateReplaceDeleteAndReplay(t *testing.T) {
	s, sess, handler, lease, operation := mutationFixture(t)
	server := httptest.NewServer(handler)
	defer server.Close()
	remote, err := syncclient.NewRemote(server.URL, "client-fixture-alice", false)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	first := *operation.After
	result, err := remote.Apply(t.Context(), sess.ID, lease, operation, strings.NewReader("new\r\n\x00"))
	if err != nil || result.Status != "applied" || result.Recovery {
		t.Fatal(result, err)
	}
	file := filepath.Join(s.workspaceDir(sess), "file")
	// A successfully applied ID is a receipt, not permission to overwrite again.
	if err = os.WriteFile(file, []byte("editor"), 0644); err != nil {
		t.Fatal(err)
	}
	result, err = remote.Apply(t.Context(), sess.ID, lease, operation, strings.NewReader("new\r\n\x00"))
	if err != nil || !result.Replayed {
		t.Fatal(result, err)
	}
	if body, _ := os.ReadFile(file); string(body) != "editor" {
		t.Fatal("replay overwrote editor")
	}
	changed := operation
	changed.After = mutationEntry("other")
	if _, err = remote.Apply(t.Context(), sess.ID, lease, changed, strings.NewReader("other")); err == nil {
		t.Fatal("ID reused for different intent")
	}
	operation.ID = strings.Repeat("b", 32)
	operation.Before = mutationEntry("editor")
	operation.After = &first
	result, err = remote.Apply(t.Context(), sess.ID, lease, operation, strings.NewReader("new\r\n\x00"))
	if err != nil || !result.Recovery {
		t.Fatal(result, err)
	}
	status, err := remote.Operation(t.Context(), sess.ID, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := remote.Recovery(t.Context(), sess.ID, status)
	if err != nil {
		t.Fatal(err)
	}
	saved, readErr := io.ReadAll(recovered)
	recovered.Close()
	if readErr != nil || string(saved) != "editor" {
		t.Fatal("remote recovery", string(saved), readErr)
	}
	backup := filepath.Join(s.sessionDir(sess), "client-sync", operation.ID, "before")
	if body, err := os.ReadFile(backup); err != nil || string(body) != "editor" {
		t.Fatal("backup", string(body), err)
	}
	operation.ID = strings.Repeat("c", 32)
	operation.Kind = "delete"
	operation.Before = &first
	operation.After = nil
	result, err = remote.Apply(t.Context(), sess.ID, lease, operation, nil)
	if err != nil || !result.Recovery {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("not deleted", err)
	}
	if err = os.WriteFile(file, []byte("recreated"), 0644); err != nil {
		t.Fatal(err)
	}
	result, err = remote.Apply(t.Context(), sess.ID, lease, operation, nil)
	if err != nil || !result.Replayed {
		t.Fatal(result, err)
	}
	if body, _ := os.ReadFile(file); string(body) != "recreated" {
		t.Fatal("replayed delete removed new file")
	}
	// Restart invalidates all leases, while the on-disk receipt still survives.
	s.clientLeases = syncproto.NewLeases()
	if _, err = remote.Apply(t.Context(), sess.ID, lease, operation, nil); err == nil {
		t.Fatal("pre-restart lease accepted")
	}
	lease, err = remote.Acquire(t.Context(), sess.ID, operation.Project, operation.Device)
	if err != nil {
		t.Fatal(err)
	}
	operation.Generation = lease.Generation
	result, err = remote.Apply(t.Context(), sess.ID, lease, operation, nil)
	if err != nil || !result.Replayed {
		t.Fatal("new lease lost durable receipt", result, err)
	}
}

func TestSyncMutationRejectsInvalidConditionsWithoutModifying(t *testing.T) {
	for _, kind := range []string{"hash", "truncated", "extra", "before", "missing_lease", "foreign", "unauthenticated", "symlink", "hardlink", "rules", "revision", "ignored"} {
		t.Run(kind, func(t *testing.T) {
			s, sess, handler, lease, operation := mutationFixture(t)
			file := filepath.Join(s.workspaceDir(sess), "file")
			if err := os.WriteFile(file, []byte("keep"), 0644); err != nil {
				t.Fatal(err)
			}
			operation.Before = mutationEntry("keep")
			body := "new\r\n\x00"
			user := "alice"
			var err error
			switch kind {
			case "hash":
				body = "bad\r\n\x00"
			case "truncated":
				body = "new"
			case "extra":
				body += "extra"
			case "before":
				operation.Before = mutationEntry("stale")
			case "missing_lease":
				lease.Token = ""
			case "foreign":
				user = "bob"
			case "unauthenticated":
				user = ""
			case "symlink":
				err = os.Symlink("file", filepath.Join(s.workspaceDir(sess), "link"))
				operation.Path = "link"
			case "hardlink":
				err = os.Link(file, filepath.Join(s.workspaceDir(sess), "alias"))
			case "rules":
				operation.RulesHash = strings.Repeat("0", 64)
			case "revision":
				operation.Revision++
			case "ignored":
				operation.Path = ".git/file"
			}
			if err != nil {
				t.Fatal(err)
			}
			w := mutationRequest(handler, operation, lease, strings.NewReader(body), user)
			if w.Code == 200 {
				t.Fatal("invalid mutation accepted", w.Body.String())
			}
			if value, _ := os.ReadFile(file); string(value) != "keep" {
				t.Fatal("rejected write changed target")
			}
		})
	}
}

type mutationCallbackReader struct {
	before func()
	source io.Reader
}

func (r *mutationCallbackReader) Read(p []byte) (int, error) {
	if r.before != nil {
		r.before()
		r.before = nil
	}
	return r.source.Read(p)
}
func TestSyncMutationFencesLeaseLostDuringUpload(t *testing.T) {
	s, sess, handler, lease, operation := mutationFixture(t)
	body := &mutationCallbackReader{before: func() {
		if err := s.syncLeases().Release(sess.ID, operation.Project, ".", lease.Device, lease.Token, lease.Generation); err != nil {
			t.Fatal(err)
		}
		if _, err := s.syncLeases().Acquire(sess.ID, operation.Project, ".", "other-device"); err != nil {
			t.Fatal(err)
		}
	}, source: strings.NewReader("new\r\n\x00")}
	w := mutationRequest(handler, operation, lease, body, "alice")
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.workspaceDir(sess), "file")); !os.IsNotExist(err) {
		t.Fatal("stale writer published", err)
	}
}

func TestSyncMutationUncertainRecordNeverReexecutes(t *testing.T) {
	s, sess, handler, lease, operation := mutationFixture(t)
	file := filepath.Join(s.workspaceDir(sess), "file")
	if err := os.WriteFile(file, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	operation.Before = mutationEntry("keep")
	root, err := s.openDataDir(s.workspaceDir(sess))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	statePath := filepath.Join(s.sessionDir(sess), "client-sync")
	if err = os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := safefs.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	calls := 0
	// Fail immediately before publication, after a durable backup was written.
	check := func() error {
		calls++
		if calls == 2 {
			return context.Canceled
		}
		return nil
	}
	result, err := applyClientMutation(t.Context(), root, journal, operation, strings.NewReader("new\r\n\x00"), check)
	if !errors.Is(err, errClientUncertain) || !result.Recovery {
		t.Fatal(result, err)
	}
	if body, _ := os.ReadFile(file); string(body) != "keep" {
		t.Fatal("canceled write published")
	}
	// Simulate a crash after publication but before recording 'applied'. Either
	// outcome has the same durable uncertain receipt and must not be guessed.
	if err = os.WriteFile(file, []byte("new\r\n\x00"), 0644); err != nil {
		t.Fatal(err)
	}
	w := mutationRequest(handler, operation, lease, strings.NewReader("new\r\n\x00"), "alice")
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"status":"uncertain"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if body, _ := os.ReadFile(file); !bytes.Equal(body, []byte("new\r\n\x00")) {
		t.Fatal("uncertain retry changed file")
	}
	backup := filepath.Join(statePath, operation.ID, "before")
	if body, _ := os.ReadFile(backup); string(body) != "keep" {
		t.Fatal("recovery lost")
	}
}

func TestSyncMutationRecoveryAPIAndOwnership(t *testing.T) {
	s, sess, handler, lease, operation := mutationFixture(t)
	if err := os.WriteFile(filepath.Join(s.workspaceDir(sess), "file"), []byte("old\r\n\x00"), 0755); err != nil {
		t.Fatal(err)
	}
	operation.Before = mutationEntry("old\r\n\x00")
	operation.Before.Executable = true
	w := mutationRequest(handler, operation, lease, strings.NewReader("new\r\n\x00"), "alice")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	base := "/api/sessions/s1/sync/operations/" + operation.ID
	for _, suffix := range []string{"", "/before"} {
		w = clientRequest(handler, "bob", "GET", base+suffix, "")
		if w.Code != 404 {
			t.Fatal("foreign recovery", w.Code)
		}
		w = clientRequest(handler, "", "GET", base+suffix, "")
		if w.Code != 401 {
			t.Fatal("unauthenticated recovery", w.Code)
		}
	}
	w = clientRequest(handler, "alice", "GET", base, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"applied"`) || strings.Contains(w.Body.String(), lease.Token) {
		t.Fatal(w.Code, w.Body.String())
	}
	// Recovery is accessible without a live lease and even after project removal.
	if err := s.syncLeases().Release(sess.ID, operation.Project, ".", lease.Device, lease.Token, lease.Generation); err != nil {
		t.Fatal(err)
	}
	if err := s.store.DeleteClientProject(sess.ID, operation.Project, operation.Revision); err != nil {
		t.Fatal(err)
	}
	w = clientRequest(handler, "alice", "GET", base+"/before", "")
	if w.Code != 200 || w.Body.String() != "old\r\n\x00" || w.Header().Get("ETag") != `"`+operation.Before.Hash+`"` {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := os.WriteFile(filepath.Join(s.sessionDir(sess), "client-sync", operation.ID, "before"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	w = clientRequest(handler, "alice", "GET", base+"/before", "")
	if w.Code != 409 {
		t.Fatal("corrupt recovery accepted", w.Code)
	}
}

func TestSyncMutationUncertainAfterPublicationAndQuota(t *testing.T) {
	for _, kind := range []string{"cancel_after_publish", "recovery_quota", "journal_quota", "concurrent_editor"} {
		t.Run(kind, func(t *testing.T) {
			s, sess, _, _, operation := mutationFixture(t)
			file := filepath.Join(s.workspaceDir(sess), "file")
			if err := os.WriteFile(file, []byte("old"), 0644); err != nil {
				t.Fatal(err)
			}
			operation.Before = mutationEntry("old")
			root, err := s.openDataDir(s.workspaceDir(sess))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			statePath := filepath.Join(s.sessionDir(sess), "client-sync")
			if err = os.Mkdir(statePath, 0700); err != nil {
				t.Fatal(err)
			}
			journal, err := safefs.Open(statePath)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			if kind == "recovery_quota" {
				dir := filepath.Join(statePath, strings.Repeat("d", 32))
				if err = os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				f, err := os.Create(filepath.Join(dir, "before"))
				if err != nil {
					t.Fatal(err)
				}
				err = f.Truncate(clientRecoveryBytes)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "journal_quota" {
				for i := 0; i < clientJournalLimit; i++ {
					if err = os.Mkdir(filepath.Join(statePath, fmt.Sprintf("%032x", i)), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			check := func() error {
				calls++
				if kind == "cancel_after_publish" && calls == 3 {
					cancel()
					return nil
				}
				if kind == "concurrent_editor" && calls == 2 {
					if err := os.WriteFile(file, []byte("editor"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}
			result, err := applyClientMutation(ctx, root, journal, operation, strings.NewReader("new\r\n\x00"), check)
			want := "old"
			switch kind {
			case "cancel_after_publish":
				if !errors.Is(err, errClientUncertain) || !result.Recovery {
					t.Fatal(result, err)
				}
				want = "new\r\n\x00"
				result, err = applyClientMutation(t.Context(), root, journal, operation, strings.NewReader("new\r\n\x00"), func() error { return nil })
				if !errors.Is(err, errClientUncertain) || !result.Replayed {
					t.Fatal("uncertain replay", result, err)
				}
			case "concurrent_editor":
				want = "editor"
				if !errors.Is(err, errClientUncertain) {
					t.Fatal(result, err)
				}
			default:
				if !errors.Is(err, syncproto.ErrLimit) {
					t.Fatal(result, err)
				}
			}
			if content, _ := os.ReadFile(file); string(content) != want {
				t.Fatalf("unexpected content %q", content)
			}
		})
	}
}

func TestSyncMutationQueryTokenCannotWrite(t *testing.T) {
	_, _, handler, lease, operation := mutationFixture(t)
	raw, _ := json.Marshal(operation)
	r := httptest.NewRequest("POST", "/api/sessions/s1/sync/apply?token=client-fixture-alice", strings.NewReader("new\r\n\x00"))
	r.Header.Set("X-Agentbox-Sync-Lease", lease.Token)
	r.Header.Set("X-Agentbox-Sync-Request", base64.RawURLEncoding.EncodeToString(raw))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("query write token accepted", w.Code)
	}
}

func TestSyncMutationDirectoriesAreEmptyOnlyAndReplaySafe(t *testing.T) {
	s, sess, handler, lease, operation := mutationFixture(t)
	directory := &syncproto.Entry{Kind: "directory"}
	operation.Kind = "mkdir"
	operation.Path = "sub"
	operation.After = directory
	w := mutationRequest(handler, operation, lease, nil, "alice")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	sub := filepath.Join(s.workspaceDir(sess), "sub")
	info, err := os.Stat(sub)
	if err != nil || !info.IsDir() {
		t.Fatal(info, err)
	}
	if runtime.GOOS == "linux" {
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != 1000 || st.Gid != 1000 {
			t.Fatal("directory ownership", st.Uid, st.Gid)
		}
	}
	if err = os.WriteFile(filepath.Join(sub, "keep"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	operation.Kind = "rmdir"
	operation.Before = directory
	operation.After = nil
	operation.ID = strings.Repeat("b", 32)
	w = mutationRequest(handler, operation, lease, nil, "alice")
	if w.Code != 409 {
		t.Fatal("nonempty directory accepted", w.Code, w.Body.String())
	}
	if body, _ := os.ReadFile(filepath.Join(sub, "keep")); string(body) != "keep" {
		t.Fatal("recursive removal")
	}
	if err = os.Remove(filepath.Join(sub, "keep")); err != nil {
		t.Fatal(err)
	}
	w = mutationRequest(handler, operation, lease, nil, "alice")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = os.Stat(sub); !os.IsNotExist(err) {
		t.Fatal("rmdir failed", err)
	}
	// Replaying an old receipt cannot delete a new directory at the same path.
	if err = os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(sub, "new"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	w = mutationRequest(handler, operation, lease, nil, "alice")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"replayed":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if body, _ := os.ReadFile(filepath.Join(sub, "new")); string(body) != "new" {
		t.Fatal("replayed directory deletion")
	}
	// Creating a directory requires absence; no file-to-directory conversion.
	operation.ID = strings.Repeat("c", 32)
	operation.Kind = "mkdir"
	operation.Before = nil
	operation.After = directory
	operation.Path = "sub/new"
	w = mutationRequest(handler, operation, lease, nil, "alice")
	if w.Code != 409 {
		t.Fatal("mkdir replaced file", w.Code)
	}
}

func TestSyncMutationRmdirRaceCannotUnlinkReplacementFile(t *testing.T) {
	s, sess, _, _, operation := mutationFixture(t)
	operation.Kind = "rmdir"
	operation.Path = "dir"
	operation.Before = &syncproto.Entry{Kind: "directory"}
	operation.After = nil
	dir := filepath.Join(s.workspaceDir(sess), "dir")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	root, err := s.openDataDir(s.workspaceDir(sess))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	statePath := filepath.Join(s.sessionDir(sess), "client-sync")
	if err = os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := safefs.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	calls := 0
	check := func() error {
		calls++
		if calls == 3 {
			if err := os.Rename(dir, dir+"-moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dir, []byte("replacement"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	_, err = applyClientMutation(t.Context(), root, journal, operation, bytes.NewReader(nil), check)
	if !errors.Is(err, errClientUncertain) {
		t.Fatal("race accepted", err)
	}
	if body, _ := os.ReadFile(dir); string(body) != "replacement" {
		t.Fatal("rmdir unlinked replacement file")
	}
}
