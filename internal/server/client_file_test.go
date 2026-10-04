package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"agentbox/internal/store"
	"agentbox/internal/syncclient"
	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

func TestSyncHTTPDownloadPreservesBytesAndLocalRecovery(t *testing.T) {
	s, sess, handler := clientTestServer(t)
	project, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Root", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	name := "中文 ?#&+%.bin"
	content := []byte("\xef\xbb\xbfhello\r\n\x00\xff\n")
	if err = os.WriteFile(filepath.Join(s.workspaceDir(sess), name), content, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.workspaceDir(sess), "empty"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	remote, err := syncclient.NewRemote(httpServer.URL, "client-fixture-alice", false)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	manifest, err := remote.Manifest(t.Context(), sess.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := remote.Acquire(t.Context(), sess.ID, project.ID, "fixture-device")
	if err != nil {
		t.Fatal(err)
	}
	lease, err = remote.Renew(t.Context(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = remote.Release(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	if _, err = remote.Renew(t.Context(), lease); err == nil {
		t.Fatal("released lease renewed")
	}
	// Resolve only this synthetic fixture's trusted temp path. Product paths must
	// still be opened component by component without symlink normalization.
	localDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	local, err := syncfs.Open(localDir)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	writer := syncfs.Writer{Root: local}
	old := syncproto.Entry{Kind: "file", Hash: syncproto.HashBytes([]byte("old")), Size: 3}
	if err = os.WriteFile(filepath.Join(localDir, name), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{name, "empty"} {
		request := syncproto.FileRequest{Project: project.ID, Revision: manifest.Revision, RulesHash: manifest.Manifest.RulesHash, Path: file, Expected: manifest.Manifest.Entries[file]}
		stream, err := remote.File(t.Context(), sess.ID, request)
		if err != nil {
			t.Fatal(err)
		}
		var before *syncproto.Entry
		if file == name {
			before = &old
		}
		recovery, err := writer.Replace(t.Context(), file, before, request.Expected, stream)
		stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		if file == name {
			saved, err := os.ReadFile(filepath.Join(localDir, filepath.FromSlash(recovery.Path)))
			if err != nil || string(saved) != "old" {
				t.Fatalf("recovery %q: %v", saved, err)
			}
		}
	}
	downloaded, err := os.ReadFile(filepath.Join(localDir, name))
	if err != nil || !bytes.Equal(downloaded, content) {
		t.Fatalf("raw bytes changed: %x %v", downloaded, err)
	}
	// The same size and mtime cannot hide a content change from a saved preview.
	oldInfo, err := os.Stat(filepath.Join(s.workspaceDir(sess), name))
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Repeat([]byte("x"), len(content))
	if err = os.WriteFile(filepath.Join(s.workspaceDir(sess), name), changed, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(filepath.Join(s.workspaceDir(sess), name), oldInfo.ModTime(), oldInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	request := syncproto.FileRequest{Project: project.ID, Revision: manifest.Revision, RulesHash: manifest.Manifest.RulesHash, Path: name, Expected: manifest.Manifest.Entries[name]}
	if stream, err := remote.File(t.Context(), sess.ID, request); !errors.Is(err, syncproto.ErrChanged) {
		if stream != nil {
			stream.Close()
		}
		t.Fatalf("stale preview: %v", err)
	}
}

func fileQuery(p store.ClientProject, name string, data []byte) url.Values {
	rules, _ := syncproto.ParseRules("")
	return url.Values{"project": {p.ID}, "project_revision": {strconv.FormatInt(p.Revision, 10)}, "rules_hash": {rules.Hash()}, "path": {name}, "hash": {syncproto.HashBytes(data)}, "size": {strconv.Itoa(len(data))}, "executable": {"false"}}
}
func TestSyncFileRejectsStaleUnsafeAndForeignReads(t *testing.T) {
	for _, kind := range []string{"revision", "rules", "mode", "symlink", "parent_link", "hardlink", "ignore_hardlink", "ignored", "traversal", "missing", "foreign", "unauthenticated", "busy"} {
		t.Run(kind, func(t *testing.T) {
			s, sess, handler := clientTestServer(t)
			p, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Root", Path: "."})
			if err != nil {
				t.Fatal(err)
			}
			dir := s.workspaceDir(sess)
			data := []byte("original")
			name := "file"
			if err = os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
				t.Fatal(err)
			}
			q := fileQuery(p, name, data)
			user := "alice"
			want := http.StatusConflict
			switch kind {
			case "revision":
				q.Set("project_revision", "2")
			case "rules":
				err = os.WriteFile(filepath.Join(dir, ".agentboxignore"), []byte("*.log\n"), 0644)
			case "mode":
				err = os.Chmod(filepath.Join(dir, name), 0755)
			case "symlink":
				err = os.Symlink(name, filepath.Join(dir, "link"))
				q.Set("path", "link")
				want = 422
			case "parent_link":
				err = os.Symlink(dir, filepath.Join(dir, "link"))
				q.Set("path", "link/file")
				want = 422
			case "hardlink":
				err = os.Link(filepath.Join(dir, name), filepath.Join(dir, "alias"))
				want = 422
			case "ignore_hardlink":
				err = os.Link(filepath.Join(dir, name), filepath.Join(dir, ".agentboxignore"))
				want = 422
			case "ignored":
				err = os.Mkdir(filepath.Join(dir, ".git"), 0700)
				if err == nil {
					err = os.WriteFile(filepath.Join(dir, ".git", "file"), data, 0644)
				}
				q.Set("path", ".git/file")
				want = 422
			case "traversal":
				q.Set("path", "../file")
				want = 400
			case "missing":
				err = os.Rename(dir, dir+"-moved")
			case "foreign":
				user = "bob"
				want = 404
			case "unauthenticated":
				user = ""
				want = 401
			case "busy":
				s.clientManifestOnce.Do(func() { s.clientManifestSlots = make(chan struct{}, 2) })
				s.clientManifestSlots <- struct{}{}
				s.clientManifestSlots <- struct{}{}
				want = 429
			}
			if err != nil {
				t.Fatal(err)
			}
			w := clientRequest(handler, user, "GET", "/api/sessions/s1/sync/file?"+q.Encode(), "")
			if w.Code != want || bytes.Contains(w.Body.Bytes(), data) {
				t.Fatalf("%s: %d %s", kind, w.Code, w.Body.String())
			}
		})
	}
}

// The peer edits the remote file as soon as headers are sent. The body must
// still be the verified snapshot rather than a newly opened mutable file.
type editOnHeader struct {
	*httptest.ResponseRecorder
	edit func()
}

func (w *editOnHeader) WriteHeader(code int) { w.edit(); w.ResponseRecorder.WriteHeader(code) }
func TestSyncFileResponseIsSnapshot(t *testing.T) {
	s, sess, handler := clientTestServer(t)
	p, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Root", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.workspaceDir(sess), "file")
	if err = os.WriteFile(file, []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/sessions/s1/sync/file?"+fileQuery(p, "file", []byte("before")).Encode(), nil)
	r.Header.Set("Authorization", "Bearer client-fixture-alice")
	w := &editOnHeader{httptest.NewRecorder(), func() {
		if err := os.WriteFile(file, []byte("after!"), 0644); err != nil {
			t.Fatal(err)
		}
	}}
	handler.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "before" {
		t.Fatalf("snapshot %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cache enabled")
	}
}

func TestSyncSnapshotStopsGrowingAtExpectedSize(t *testing.T) {
	snapshot := boundedSnapshot{remaining: 3}
	_, err := io.Copy(&snapshot, bytes.NewReader([]byte("large")))
	if !errors.Is(err, syncproto.ErrChanged) || snapshot.buffer.Len() != 0 {
		t.Fatal("unbounded snapshot", err, snapshot.buffer.Len())
	}
}
