package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/store"
	"agentbox/internal/syncproto"
)

func TestManifestRejectsLinksAndMissingRootWithoutPartialSuccess(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "missing"} {
		t.Run(kind, func(t *testing.T) {
			s, sess, handler := clientTestServer(t)
			dir := s.workspaceDir(sess)
			if err := os.WriteFile(filepath.Join(dir, "file"), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Root", Path: "."})
			if err != nil {
				t.Fatal(err)
			}
			want := 422
			switch kind {
			case "symlink":
				err = os.Symlink(t.TempDir(), filepath.Join(dir, "link"))
			case "hardlink":
				err = os.Link(filepath.Join(dir, "file"), filepath.Join(dir, "link"))
			case "missing":
				err = os.Rename(dir, dir+"-moved")
				want = 409
			}
			if err != nil {
				t.Fatal(err)
			}
			w := clientRequest(handler, "alice", "GET", "/api/sessions/s1/sync/manifest?project="+p.ID, "")
			if w.Code != want || strings.Contains(w.Body.String(), `"manifest"`) {
				t.Fatalf("%s: %d %s", kind, w.Code, w.Body.String())
			}
		})
	}
}

func TestManifestIgnoresExcludedTreesAndPreservesRawBytes(t *testing.T) {
	s, sess, handler := clientTestServer(t)
	dir := s.workspaceDir(sess)
	if err := os.Mkdir(filepath.Join(dir, "node_modules"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "node_modules", "link")); err != nil {
		t.Fatal(err)
	}
	content := []byte("hello\r\n\x00")
	for name, body := range map[string][]byte{"file": content, "skip.log": []byte("excluded"), ".agentboxignore": []byte("*.log\n")} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Root", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	w := clientRequest(handler, "alice", "GET", "/api/sessions/s1/sync/manifest?project="+p.ID, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response struct {
		Manifest syncproto.Manifest
		Digest   string
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Manifest.Entries) != 2 || response.Manifest.Entries["file"].Hash != syncproto.HashBytes(content) {
		t.Fatalf("manifest contents: %+v", response)
	}
	digest, err := response.Manifest.Digest()
	if err != nil || digest != response.Digest {
		t.Fatal("incorrect digest", err)
	}
	// A deleted resource, including one requested with a valid old project ID,
	// cannot reappear merely because a stale manifest request arrives.
	if err = s.store.Delete(sess.ID); err != nil {
		t.Fatal(err)
	}
	if w = clientRequest(handler, "alice", "GET", "/api/sessions/s1/sync/manifest?project="+p.ID, ""); w.Code != 404 {
		t.Fatal("purged metadata accepted", w.Code)
	}
}

func TestClientLeaseRouteFencesProjectEditAndSeparatesUsers(t *testing.T) {
	s, sess, handler := clientTestServer(t)
	p, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Root", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"project":"` + p.ID + `","device":"test-device","action":"acquire"}`
	w := clientRequest(handler, "alice", "POST", "/api/sessions/s1/sync/lease", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var lease syncproto.Lease
	if err = json.Unmarshal(w.Body.Bytes(), &lease); err != nil {
		t.Fatal(err)
	}
	if w = clientRequest(handler, "bob", "POST", "/api/sessions/s1/sync/lease", body); w.Code != 404 {
		t.Fatal("cross-user lease", w.Code)
	}
	if w = clientRequest(handler, "", "POST", "/api/sessions/s1/sync/lease?token=client-fixture-alice", body); w.Code != 401 {
		t.Fatal("query token on lease write", w.Code)
	}
	if w = clientRequest(handler, "alice", "DELETE", "/api/sessions/s1/client-projects/"+p.ID+"?revision=1", ""); w.Code != 409 {
		t.Fatal("removed leased project", w.Code)
	}
	if w = clientRequest(handler, "alice", "PUT", "/api/sessions/s1/client-projects/"+p.ID, `{"name":"Changed","path":".","arguments":[],"revision":1}`); w.Code != 409 {
		t.Fatal("modified leased mapping", w.Code)
	}
	for _, action := range []string{"renew", "release"} {
		body := `{"project":"` + p.ID + `","device":"test-device","action":"` + action + `","generation":"` + lease.Generation + `"}`
		if w = clientRequest(handler, "alice", "POST", "/api/sessions/s1/sync/lease", body); w.Code != 409 {
			t.Fatal("missing lease token accepted", w.Code)
		}
		r := httptest.NewRequest("POST", "/api/sessions/s1/sync/lease", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer client-fixture-alice")
		r.Header.Set("X-Agentbox-Sync-Lease", lease.Token)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(action, w.Code, w.Body.String())
		}
	}
	if w = clientRequest(handler, "alice", "DELETE", "/api/sessions/s1/client-projects/"+p.ID+"?revision=1", ""); w.Code != 200 {
		t.Fatal("release did not unblock metadata", w.Code)
	}
}
