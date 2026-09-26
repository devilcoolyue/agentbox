package gitaccess

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForgeEndpointsAndLinks(t *testing.T) {
	tests := []struct{ provider, base, repo, want string }{
		{"github", "https://github.com", "https://github.com/owner/repo.git", "https://api.github.com/repos/owner/repo"},
		{"github", "https://github.corp", "https://github.corp/owner/repo.git", "https://github.corp/api/v3/repos/owner/repo"},
		{"gitlab", "https://git.corp/gitlab", "https://git.corp/gitlab/group/subgroup/repo.git", "https://git.corp/gitlab/api/v4/projects/group%2Fsubgroup%2Frepo"},
	}
	for _, tt := range tests {
		f, err := NewForge(tt.provider, tt.base, tt.repo, "test", "pat", http.DefaultClient)
		if err != nil || f.root() != tt.want {
			t.Fatalf("endpoint: %+v %v", f, err)
		}
		for _, bad := range []string{"javascript:alert(1)", "https://evil.example/repo", "https://secret@github.com/owner/repo/pull/1", tt.base + "/other/repo/pull/1"} {
			if f.reviewURL(bad) != "" {
				t.Fatalf("accepted link %q", bad)
			}
		}
	}
}
func TestForgeNeverRedirectsCredential(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer up.Close()
	f, err := NewForge("github", up.URL, up.URL+"/o/r.git", "synthetic-secret", "pat", up.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.DefaultBranch(context.Background())
	if err == nil || hits != 0 || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("redirect: %v %d", err, hits)
	}
}
