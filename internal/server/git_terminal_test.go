package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func TestGitTerminalScopeRevocationAndHelper(t *testing.T) {
	s, sess := newGitTestServer(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.homeDir(sess), 0700); err != nil {
		t.Fatal(err)
	}
	s.cfg.ProxyBridge = config.ProxyBridgeConfig{Bind: "127.0.0.1:0", Host: "127.0.0.1"}
	gitCmd(t, s.workspaceDir(sess), "init", "-q", "-b", "main")
	gitCmd(t, s.workspaceDir(sess), "remote", "add", "origin", "https://git.example.com/team/repo.git")
	c := createGitConnection(t, s, sess.User, "https://git.example.com", false)
	b := store.GitBinding{SessionID: sess.ID, Repo: "", Remote: "origin", URL: "https://git.example.com/team/repo.git", ConnectionID: c.ID}
	if err := s.store.SaveGitBinding(sess.User, b, 0); err != nil {
		t.Fatal(err)
	}
	login := "fixture-terminal-login"
	if err := s.store.CreateToken(login, sess.User); err != nil {
		t.Fatal(err)
	}
	create := func(write bool) (*gitTerminalGrant, map[string]string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"repo": "", "remote": "origin", "write": write})
		r := accessRequest(sess.User, "POST", "/git/terminal", string(payload))
		r.Header.Set("Authorization", "Bearer "+login)
		w := httptest.NewRecorder()
		s.handleGitTerminal(w, r, sess)
		if w.Code != 201 {
			t.Fatalf("grant: %d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), login) || strings.Contains(w.Body.String(), "token") || strings.Contains(w.Body.String(), "synthetic-secret") {
			t.Fatal("grant response leaked secret")
		}
		var response gitTerminalGrant
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		s.gitTerminal.mu.Lock()
		g := s.gitTerminal.grants[response.ID]
		s.gitTerminal.mu.Unlock()
		raw, err := os.ReadFile(filepath.Join(s.homeDir(sess), ".agentbox-git", g.ID, "grant.json"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), login) || strings.Contains(string(raw), "synthetic-secret") {
			t.Fatal("long-lived secret installed")
		}
		var data map[string]json.RawMessage
		if err = json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		conf := map[string]string{}
		for _, k := range []string{"url", "token"} {
			var value string
			if err = json.Unmarshal(data[k], &value); err != nil {
				t.Fatal(err)
			}
			conf[k] = value
		}
		return g, conf
	}
	call := func(g *gitTerminalGrant, conf map[string]string, action, payload, token string) int {
		t.Helper()
		r, err := http.NewRequest("POST", conf["url"]+"/"+action, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	g, conf := create(false)
	// Expiry is enforced independently of the listener shutdown timer.
	expired := *g
	expired.Expires = time.Now().Add(-time.Second)
	expiryRequest := accessRequest(sess.User, "POST", "/status", `{"request_id":"0123456789abcdef"}`)
	expiryRequest.Header.Set("Authorization", "Bearer "+conf["token"])
	expiryResponse := httptest.NewRecorder()
	s.serveGitTerminal(&expired, expiryResponse, expiryRequest)
	if expiryResponse.Code != 410 {
		t.Fatal("expired grant accepted")
	}
	other := sess
	other.User = "bob"
	revokeRequest := accessRequest("bob", "DELETE", "/terminal/"+g.ID, "")
	revokeRequest.SetPathValue("grant", g.ID)
	revokeResponse := httptest.NewRecorder()
	s.handleGitTerminal(revokeResponse, revokeRequest, other)
	if revokeResponse.Code != 404 || g.ctx.Err() != nil {
		t.Fatal("another user revoked grant")
	}

	payload := `{"request_id":"0123456789abcdef"}`
	for _, tc := range []struct {
		action, body, token string
		want                int
	}{
		{"status", payload, conf["token"], 200}, {"status", payload, "bad", 401}, {"push", payload, conf["token"], 403}, {"push-preview", payload, conf["token"], 403},
		{"clone", payload, conf["token"], 404}, {"status", `{"request_id":"0123456789abcdef","repo":"other"}`, conf["token"], 400},
		{"status", `{"request_id":"short"}`, conf["token"], 400},
	} {
		if got := call(g, conf, tc.action, tc.body, tc.token); got != tc.want {
			t.Fatalf("%s: %d want %d", tc.action, got, tc.want)
		}
	}
	helper := filepath.Join(s.homeDir(sess), ".agentbox-git", g.ID, "abox-git")
	cmd := exec.CommandContext(t.Context(), "python3", "-I", helper, "status")
	cmd.Env = append(os.Environ(), "HTTP_PROXY=http://127.0.0.1:1", "ALL_PROXY=http://127.0.0.1:1")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), `"branch": "main"`) {
		t.Fatalf("helper: %s %v", out, err)
	}
	if err := s.store.DeleteToken(login); err != nil {
		t.Fatal(err)
	}
	if got := call(g, conf, "status", payload, conf["token"]); got != 401 {
		t.Fatal("logout kept grant alive", got)
	}
	if err := s.store.CreateToken(login, sess.User); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SaveGitBinding(sess.User, store.GitBinding{SessionID: sess.ID, Remote: "origin"}, 1); err != nil {
		t.Fatal(err)
	}
	if got := call(g, conf, "status", payload, conf["token"]); got != 403 {
		t.Fatal("unbound grant accepted", got)
	}
	if err := s.store.SaveGitBinding(sess.User, b, 0); err != nil {
		t.Fatal(err)
	}
	stored, err := s.store.GitConnection(sess.User, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Enabled = false
	if _, err = s.store.SaveGitConnection(stored, stored.Revision); err != nil {
		t.Fatal(err)
	}
	if got := call(g, conf, "status", payload, conf["token"]); got != 403 {
		t.Fatal("disabled connection accepted", got)
	}
	r := accessRequest(sess.User, "DELETE", "/git/terminal/"+g.ID, "")
	r.SetPathValue("grant", g.ID)
	w := httptest.NewRecorder()
	s.handleGitTerminal(w, r, sess)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	select {
	case <-g.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("revoke did not cancel")
	}
}
