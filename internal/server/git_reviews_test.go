package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agentbox/internal/store"
)

func TestGitReviewProvidersPreviewCreateAndDuplicate(t *testing.T) {
	for _, provider := range []string{"github", "gitlab"} {
		t.Run(provider, func(t *testing.T) {
			s, sess := newGitTestServer(t)
			if err := s.store.Put(sess); err != nil {
				t.Fatal(err)
			}
			ws := s.workspaceDir(sess)
			gitCmd(t, ws, "init", "-q", "-b", "feature/example")
			if err := os.WriteFile(filepath.Join(ws, "file"), []byte("fixture"), 0644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, ws, "add", "-A")
			gitCmd(t, ws, "commit", "-q", "-m", "fixture")
			output, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			head := strings.TrimSpace(string(output))
			target := strings.Repeat("a", 40)
			var creates atomic.Int32
			var serverURL string
			permissionDenied := false
			review := func() map[string]any {
				if provider == "github" {
					return map[string]any{"number": 12, "title": "Review <script>literal</script>", "html_url": serverURL + "/team/repo/pull/12", "state": "open", "draft": true, "head": map[string]any{"ref": "feature/example", "repo": map[string]string{"full_name": "team/repo"}}, "base": map[string]string{"ref": "main"}}
				}
				return map[string]any{"iid": 12, "title": "Draft: Review", "web_url": serverURL + "/team/repo/-/merge_requests/12", "state": "opened", "draft": true, "source_branch": "feature/example", "target_branch": "main", "source_project_id": 1, "target_project_id": 1}
			}
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if permissionDenied {
					w.WriteHeader(403)
					w.Write([]byte("synthetic-secret must never appear"))
					return
				}
				if provider == "github" {
					if r.Header.Get("Authorization") != "Bearer synthetic-secret" {
						t.Error("missing GitHub API auth")
					}
				} else if r.Header.Get("PRIVATE-TOKEN") != "synthetic-secret" {
					t.Error("missing GitLab PAT auth")
				}
				w.Header().Set("Content-Type", "application/json")
				root := "/api/v3/repos/team/repo"
				if provider == "gitlab" {
					root = "/api/v4/projects/team/repo"
				}
				suffix := strings.TrimPrefix(r.URL.Path, root)
				switch {
				case suffix == "":
					json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
				case strings.Contains(suffix, "/branches/"):
					name := strings.SplitN(suffix, "/branches/", 2)[1]
					sha := head
					if name == "main" {
						sha = target
					}
					commit := map[string]string{"sha": sha}
					if provider == "gitlab" {
						commit = map[string]string{"id": sha}
					}
					json.NewEncoder(w).Encode(map[string]any{"name": name, "protected": name == "main", "commit": commit})
				case (suffix == "/pulls" || suffix == "/merge_requests") && r.Method == "GET":
					if creates.Load() > 0 {
						json.NewEncoder(w).Encode([]any{review()})
					} else {
						ioEmpty := []any{}
						json.NewEncoder(w).Encode(ioEmpty)
					}
				case (suffix == "/pulls" || suffix == "/merge_requests") && r.Method == "POST":
					creates.Add(1)
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if provider == "github" {
						if body["head"] != "feature/example" || body["base"] != "main" || body["draft"] != true {
							t.Errorf("PR payload: %#v", body)
						}
					} else if body["source_branch"] != "feature/example" || body["target_branch"] != "main" || body["remove_source_branch"] != false || !strings.HasPrefix(fmt.Sprint(body["title"]), "Draft: ") {
						t.Errorf("MR payload: %#v", body)
					}
					w.WriteHeader(201)
					json.NewEncoder(w).Encode(review())
				default:
					t.Errorf("unexpected API request %s %s", r.Method, r.URL.String())
					w.WriteHeader(404)
				}
			}))
			defer upstream.Close()
			serverURL = upstream.URL
			c := createGitConnection(t, s, sess.User, upstream.URL, false)
			// Fixture helper uses GitLab by default; save a GitHub connection directly
			// through its HTTP creation interface for the GitHub case.
			if provider == "github" {
				raw, _ := json.Marshal(map[string]any{"label": "GitHub fixture", "provider": "github", "base_url": upstream.URL, "token": "synthetic-secret", "read_only": false})
				w := httptest.NewRecorder()
				s.handleGitConnections(w, accessRequest(sess.User, "POST", "/git/connections", string(raw)))
				if w.Code != 201 {
					t.Fatal(w.Body.String())
				}
				json.Unmarshal(w.Body.Bytes(), &c)
			}
			s.gitHTTPClient = func(context.Context, store.GitConnection) (*http.Client, func(), error) {
				return upstream.Client(), func() {}, nil
			}
			gitCmd(t, ws, "remote", "add", "origin", upstream.URL+"/team/repo.git")
			b := store.GitBinding{SessionID: sess.ID, Remote: "origin", URL: upstream.URL + "/team/repo.git", ConnectionID: c.ID}
			if err := s.store.SaveGitBinding(sess.User, b, 0); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			s.handleGitReviews(w, httptest.NewRequest("GET", "/reviews?remote=origin", nil), sess)
			if w.Code != 200 || !strings.Contains(w.Body.String(), head) {
				t.Fatalf("list: %d %s", w.Code, w.Body.String())
			}
			request := gitReviewRequest{Remote: "origin", Title: "Review", Body: "Fixture description", Source: "feature/example", Target: "main", Draft: true, ExpectedHead: head}
			call := func(preview bool) *httptest.ResponseRecorder {
				raw, _ := json.Marshal(request)
				w := httptest.NewRecorder()
				s.gitReviewWrite(w, httptest.NewRequest("POST", "/reviews", strings.NewReader(string(raw))), sess, preview)
				return w
			}
			if w = call(true); w.Code != 200 || creates.Load() != 0 || !strings.Contains(w.Body.String(), `"protected":true`) {
				t.Fatalf("preview: %d %s", w.Code, w.Body.String())
			}
			request.ExpectedTarget = strings.Repeat("b", 40)
			if w = call(false); w.Code != 409 || creates.Load() != 0 {
				t.Fatal("changed target created a review")
			}
			request.ExpectedTarget = target
			if w = call(false); w.Code != 201 || creates.Load() != 1 {
				t.Fatalf("create: %d %s", w.Code, w.Body.String())
			}
			if w = call(false); w.Code != 200 || creates.Load() != 1 || !strings.Contains(w.Body.String(), `"existing":true`) {
				t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
			}
			permissionDenied = true
			if w = call(true); w.Code != 502 || strings.Contains(w.Body.String(), "synthetic-secret") {
				t.Fatalf("raw API error leaked: %s", w.Body.String())
			}
			permissionDenied = false
			c, err = s.store.GitConnection(sess.User, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			c.ReadOnly = true
			if _, err = s.store.SaveGitConnection(c, c.Revision); err != nil {
				t.Fatal(err)
			}
			if w = call(false); w.Code != 409 || creates.Load() != 1 {
				t.Fatal("read-only connection created review")
			}
		})
	}
}
