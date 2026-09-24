package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/credentials"
)

func TestCodexOAuthRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	acct := config.Account{ID: "codex-oauth-test", Type: config.AgentCodex, CredentialsDir: t.TempDir()}
	s.cfg.Accounts = []config.Account{acct}
	call := func(handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.SetPathValue("id", acct.ID)
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	start := call(s.handleOAuthStart, "")
	var response struct {
		URL string `json:"url"`
	}
	json.Unmarshal(start.Body.Bytes(), &response)
	link, err := url.Parse(response.URL)
	if err != nil || link.Host != "auth.openai.com" {
		t.Fatalf("bad authorize URL: %s", start.Body.String())
	}
	q := link.Query()
	if q.Get("redirect_uri") != codexOAuthRedirect || q.Get("client_id") != codexOAuthClientID || q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "offline_access") {
		t.Fatal("invalid OAuth parameters")
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != "synthetic-code" || r.Form.Get("redirect_uri") != codexOAuthRedirect || r.Form.Get("client_id") != codexOAuthClientID || base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") {
			t.Error("invalid exchange request")
		}
		fmt.Fprint(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","id_token":"e30.eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiZml4dHVyZSJ9fQ.signature"}`)
	}))
	defer upstream.Close()
	old := codexOAuthToken
	codexOAuthToken = upstream.URL
	defer func() { codexOAuthToken = old }()
	finish := func(state string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]string{"code": codexOAuthRedirect + "?" + url.Values{"code": {"synthetic-code"}, "state": {state}}.Encode()})
		return call(s.handleOAuthFinish, string(raw))
	}
	if w := finish("wrong-state"); w.Code != 400 || calls != 0 {
		t.Fatalf("state validation: %d, calls %d", w.Code, calls)
	}
	oauthMu.Lock()
	pending := oauthPend[acct.ID]
	pending.busy = true
	oauthPend[acct.ID] = pending
	oauthMu.Unlock()
	if w := finish(q.Get("state")); w.Code != http.StatusConflict || calls != 0 {
		t.Fatal("concurrent exchange accepted")
	}
	if w := call(s.handleOAuthStart, ""); w.Code != http.StatusConflict {
		t.Fatal("in-flight authorization replaced")
	}
	oauthMu.Lock()
	pending.busy = false
	oauthPend[acct.ID] = pending
	oauthMu.Unlock()
	w := finish(q.Get("state"))
	if w.Code != 200 {
		t.Fatalf("finish: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "synthetic-") {
		t.Fatal("token leaked")
	}
	if credentials.CodexAuthMode(acct) != "oauth" {
		t.Fatal("OAuth mode not reported")
	}
	if status, _ := credentials.Status(acct); status != "ok" {
		t.Fatal(status)
	}
	raw, _ := os.ReadFile(filepath.Join(acct.CredentialsDir, "auth.json"))
	if !strings.Contains(string(raw), `"account_id": "fixture"`) {
		t.Fatal("missing account selector")
	}
	if w := finish(q.Get("state")); w.Code != 400 || calls != 1 {
		t.Fatal("authorization code replay accepted")
	}
	// Expired sessions cannot reach the upstream.
	call(s.handleOAuthStart, "")
	oauthMu.Lock()
	p := oauthPend[acct.ID]
	p.created = time.Now().Add(-oauthPendingTTL - time.Second)
	oauthPend[acct.ID] = p
	oauthMu.Unlock()
	if w := finish(p.state); w.Code != 400 || calls != 1 {
		t.Fatal("expired authorization accepted")
	}
	oauthMu.Lock()
	delete(oauthPend, acct.ID)
	oauthMu.Unlock()
}

func TestCodexCallbackRejectsMalformedOrUnboundCode(t *testing.T) {
	for _, raw := range []string{"code", "code#state", "http://evil.test/auth/callback?code=x&state=state", codexOAuthRedirect + "?code=x", codexOAuthRedirect + "?code=x&state=state&state=other", codexOAuthRedirect + "?error=access_denied&state=state"} {
		if _, err := codexCallbackCode(raw, "state"); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestCodexOAuthBoundProxyFailureDoesNotExchange(t *testing.T) {
	s, _ := newTestServer(t)
	acct := config.Account{ID: "codex-bad-proxy", Type: config.AgentCodex, CredentialsDir: t.TempDir(), ProxyID: "missing-proxy"}
	s.cfg.Accounts = []config.Account{acct}
	oauthMu.Lock()
	oauthPend[acct.ID] = oauthPending{state: "state", verifier: "verifier", created: time.Now()}
	oauthMu.Unlock()
	defer func() { oauthMu.Lock(); delete(oauthPend, acct.ID); oauthMu.Unlock() }()
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"code":"http://localhost:1455/auth/callback?code=x&state=state"}`))
	req.SetPathValue("id", acct.ID)
	w := httptest.NewRecorder()
	s.handleOAuthFinish(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("proxy failure: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(acct.CredentialsDir, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("credentials written on proxy failure")
	}
}
