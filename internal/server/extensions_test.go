package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

const gatewayTestBind = "127.0.0.1:18082"

func extensionTestServer(t *testing.T, upstream string) (*Server, store.Session) {
	t.Helper()
	s, sess := newTestServer(t)
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"data_dir": s.cfg.DataDir, "auth_token": "test-token-long-enough", "agent_image": "test",
		"extensions": map[string]any{"gateway": map[string]any{"bind": gatewayTestBind}},
		"accounts": []config.Account{
			{ID: "relay", Label: "Relay", Type: config.AgentClaude, Env: map[string]string{envAnthropicBaseURL: upstream, envAnthropicToken: "relay-token"}},
			{ID: "oauth", Label: "Subscription", Type: config.AgentClaude},
			// No auth.json yet: not eligible until a test writes an API key.
			{ID: "codex", Label: "Codex", Type: config.AgentCodex, CredentialsDir: t.TempDir()},
		}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	if s.cfg, err = config.Load(path); err != nil {
		t.Fatal(err)
	}
	for _, u := range []store.User{{Name: "alice", Role: store.RoleUser}, {Name: "root", Role: store.RoleAdmin}} {
		if err := s.store.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	sess.AccountID, sess.Agent = "relay", config.AgentClaude
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	if err := s.initExtensions(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s.extensions.Close(ctx)
	})
	return s, sess
}

func extensionRequest(s *Server, user, method, path, body string, h http.HandlerFunc, id string) *httptest.ResponseRecorder {
	r := accessRequest(user, method, path, body)
	if id != "" {
		r.SetPathValue("id", id)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func enableLoopGuard(t *testing.T, s *Server, expected int64, accounts ...string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"expected_revision": expected, "enabled": true,
		"permissions": []string{"model.request.fingerprint", "model.stream.text"},
		"bindings":    map[string]any{"mode": "accounts", "accounts": accounts}})
	w := extensionRequest(s, "root", "PUT", "/api/extensions/agentbox.loop-guard", string(body), s.handleUpdateExtension, "agentbox.loop-guard")
	if w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	// Tests do not bind the listener; mark the gateway up as Run would.
	s.extGW.up.Store(true)
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func TestExtensionAdminAPIAndEnvRewrite(t *testing.T) {
	s, sess := extensionTestServer(t, "https://relay.example.com/api")
	w := extensionRequest(s, "root", "GET", "/api/extensions", "", s.handleExtensions, "")
	var list extensionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Extensions) != 1 {
		t.Fatalf("list: %v %s", err, w.Body.String())
	}
	lg := list.Extensions[0]
	if lg.ID != "agentbox.loop-guard" || lg.Setting.Enabled || lg.Status.State != "disabled" || lg.Setting.Bindings.Mode != "none" ||
		len(lg.Setting.Permissions) != 2 || !lg.Builtin || lg.FailurePolicy != "fail_open" {
		t.Fatalf("loop guard view: %+v", lg)
	}
	eligible := map[string]bool{}
	for _, a := range lg.Accounts {
		eligible[a.ID] = a.Eligible
	}
	if !eligible["relay"] || eligible["oauth"] || eligible["codex"] {
		t.Fatalf("eligibility: %+v", lg.Accounts)
	}
	if list.Gateway.Bind != gatewayTestBind || list.Gateway.Host != "127.0.0.1" {
		t.Fatalf("gateway view: %+v", list.Gateway)
	}

	// Enabling requires the permissions the hooks need.
	w = extensionRequest(s, "root", "PUT", "/api/extensions/agentbox.loop-guard",
		`{"expected_revision":0,"enabled":true,"permissions":["model.stream.text"],"bindings":{"mode":"all"}}`, s.handleUpdateExtension, "agentbox.loop-guard")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("enabled without permissions: %d", w.Code)
	}
	if w = extensionRequest(s, "root", "PUT", "/api/extensions/agentbox.loop-guard",
		`{"expected_revision":0,"enabled":false,"config":{"window":8},"bindings":{"mode":"none"}}`, s.handleUpdateExtension, "agentbox.loop-guard"); w.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range config accepted: %d", w.Code)
	}
	enableLoopGuard(t, s, 0, "relay")
	if w = extensionRequest(s, "root", "PUT", "/api/extensions/agentbox.loop-guard",
		`{"expected_revision":0,"enabled":false,"bindings":{"mode":"none"}}`, s.handleUpdateExtension, "agentbox.loop-guard"); w.Code != http.StatusConflict {
		t.Fatalf("stale revision: %d", w.Code)
	}
	if w = extensionRequest(s, "alice", "GET", "/api/extensions", "", s.admin(http.HandlerFunc(s.handleExtensions)).ServeHTTP, ""); w.Code == http.StatusOK {
		t.Fatal("non-admin listed extensions")
	}

	env, err := s.execEnv(sess)
	if err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:18082/x/s1/" + s.extensionMAC("s1")
	if envValue(env, envAnthropicBaseURL) != want || envValue(env, envAnthropicToken) != "relay-token" {
		t.Fatalf("base URL not rewritten: %v", env)
	}
	if v := s.view(sess); len(v.Extensions) != 1 || v.Extensions[0].ID != "agentbox.loop-guard" {
		t.Fatalf("workspace view hides the inspecting extension: %+v", v.Extensions)
	}

	// Subscription accounts are never routed through the gateway.
	oauth := sess
	oauth.ID, oauth.AccountID = "s2", "oauth"
	if err := s.store.Put(oauth); err != nil {
		t.Fatal(err)
	}
	if env, _ := s.execEnv(oauth); envValue(env, envAnthropicBaseURL) != "" || len(s.view(oauth).Extensions) != 0 {
		t.Fatal("subscription account rewritten")
	}

	// A gateway that is down leaves fail_open traffic direct, and says so.
	s.extGW.up.Store(false)
	if env, _ := s.execEnv(sess); envValue(env, envAnthropicBaseURL) != "https://relay.example.com/api" || len(s.view(sess).Extensions) != 0 {
		t.Fatal("fail_open rewrite to a gateway that is down")
	}

	// Deleting the account prunes the binding in the same config write.
	if err := s.cfg.RemoveAccount("relay"); err != nil {
		t.Fatal(err)
	}
	item := s.cfg.GetExtensions().Items["agentbox.loop-guard"]
	if len(item.Bindings.Accounts) != 0 || item.Revision != 2 {
		t.Fatalf("binding not pruned: %+v", item)
	}
}

func TestExtensionGatewayRoutesAndInterrupts(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/messages" || r.Header.Get("X-Api-Key") != "relay-token" {
			t.Errorf("bad upstream request %s key=%q", r.URL.Path, r.Header.Get("X-Api-Key"))
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 2000 && r.Context().Err() == nil; i++ {
			b, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Go.\n"}})
			io.WriteString(w, "data: "+string(b)+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	s, sess := extensionTestServer(t, upstream.URL+"/api")
	enableLoopGuard(t, s, 0, "relay")
	deadline := time.Now().Add(5 * time.Second)
	for s.extensions.Status("agentbox.loop-guard").State != "running" {
		if time.Now().After(deadline) {
			t.Fatal("extension did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	gw := httptest.NewServer(s.extGW.proxy)
	defer gw.Close()
	post := func(path, body string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", gw.URL+path, strings.NewReader(body))
		req.Header.Set("X-Api-Key", "relay-token")
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	base := "/x/" + sess.ID + "/" + s.extensionMAC(sess.ID)
	resp, body := post(base+"/v1/messages", `{"stream":true}`)
	if resp.Header.Get("X-Agentbox-Extensions") != "monitoring" || !strings.Contains(body, "model_output_loop") {
		t.Fatalf("not interrupted: %s %.200s", resp.Header.Get("X-Agentbox-Extensions"), body)
	}
	if resp, _ = post(base+"/v1/messages", `{"stream":true}`); resp.StatusCode != http.StatusUnprocessableEntity || calls.Load() != 1 {
		t.Fatalf("identical retry: %d calls=%d", resp.StatusCode, calls.Load())
	}
	events := s.extensions.Events("agentbox.loop-guard")
	if len(events) != 2 || events[1].Session != sess.ID || events[1].User != "alice" || events[1].Account != "relay" {
		t.Fatalf("events: %+v", events)
	}
	for path, status := range map[string]int{
		"/x/" + sess.ID + "/wrong-mac/v1/messages":                 http.StatusForbidden,
		"/x/missing/" + s.extensionMAC("missing") + "/v1/messages": http.StatusNotFound,
		"/v1/messages": http.StatusNotFound,
	} {
		if resp, _ := post(path, `{}`); resp.StatusCode != status {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, status)
		}
	}
	// Revoked account access is enforced on every request.
	if _, err := s.cfg.UpdateAccount("relay", config.AccountPatch{Access: &config.AccountAccess{Mode: "admin"}}); err != nil {
		t.Fatal(err)
	}
	if resp, _ := post(base+"/v1/messages", `{"stream":true,"x":1}`); resp.StatusCode != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("revoked account forwarded: %d", resp.StatusCode)
	}
}

func TestExtensionEnvNoProxyAndTmuxCleanup(t *testing.T) {
	if !hostListed("localhost, 10.0.0.5", "10.0.0.5") || hostListed("10.0.0.50", "10.0.0.5") {
		t.Fatal("hostListed")
	}
	s, sess := extensionTestServer(t, "https://relay.example.com")
	if _, err := s.cfg.SetExtension("agentbox.loop-guard", config.ExtensionSetting{Enabled: true,
		Permissions: []string{"model.request.fingerprint", "model.stream.text"}, Bindings: config.ExtensionBindings{Mode: "all"}}, 0); err != nil {
		t.Fatal(err)
	}
	s.applyExtensions()
	defer func() {
		s.extGW.mu.Lock()
		if s.extGW.ln != nil {
			s.extGW.ln.Close()
		}
		s.extGW.mu.Unlock()
	}()
	s.extGW.up.Store(true)
	acct, _ := s.cfg.Account("relay")
	env := s.extensionEnv(sess, acct, []string{"NO_PROXY=localhost,127.0.0.2", "no_proxy=localhost,127.0.0.1", envAnthropicBaseURL + "=https://relay.example.com"})
	if envValue(env, "NO_PROXY") != "localhost,127.0.0.2,127.0.0.1" || envValue(env, "no_proxy") != "localhost,127.0.0.1" {
		t.Fatalf("NO_PROXY not extended: %v", env)
	}
	if strings.Count(strings.Join(env, "\n"), envAnthropicBaseURL+"=") != 1 {
		t.Fatal("duplicate base URL")
	}
	if !strings.Contains(tmuxEnvSync(nil), "set-environment -gu ANTHROPIC_BASE_URL") {
		t.Fatal("injected base URL would linger in tmux after unbinding")
	}
}

// writeCodexRelayPool writes the pool files SaveCodexKey produces for a relay.
func writeCodexRelayPool(t *testing.T, dir, base string) []byte {
	t.Helper()
	pool := []byte("forced_login_method = \"api\"\nmodel_provider = \"agentbox\"\n\n[model_providers.agentbox]\nbase_url = \"" + base +
		"\"\nname = \"agentbox\"\nrequires_openai_auth = true\nwire_api = \"responses\"\n")
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"OPENAI_API_KEY":"codex-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), pool, 0o600); err != nil {
		t.Fatal(err)
	}
	return pool
}

func waitForFile(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never contained %q:\n%s", path, want, raw)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Codex API key / relay accounts reach the gateway through the session's
// config.toml: binding rewrites the session copy (never the pool), the gateway
// forwards to the pool's real upstream and interrupts a Responses loop, and
// unbinding restores the upstream.
func TestExtensionCodexRelayThroughSessionConfig(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer codex-key" {
			t.Errorf("bad upstream request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 2000 && r.Context().Err() == nil; i++ {
			b, _ := json.Marshal(map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": "rs_1", "output_index": 0,
				"summary_index": 0, "delta": "Checking the file again.\n"})
			io.WriteString(w, "event: response.reasoning_summary_text.delta\ndata: "+string(b)+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	s, _ := extensionTestServer(t, "https://relay.example.com")
	acct, _ := s.cfg.Account("codex")
	credDir := acct.CredentialsDir
	pool := writeCodexRelayPool(t, credDir, upstream.URL+"/v1")
	sess := store.Session{ID: "s3", User: "alice", Name: "codex", Agent: config.AgentCodex, AccountID: "codex"}
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(s.homeDir(sess), ".codex")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionConfig := filepath.Join(home, "config.toml")
	if err := os.WriteFile(sessionConfig, append([]byte("model = \"session-model\"\n"), pool...), 0o600); err != nil {
		t.Fatal(err)
	}

	enableLoopGuard(t, s, 0, "codex")
	gatewayURL := s.extensionGatewayURL(sess.ID)
	// applyExtensions reconciled the seeded session in the background; a
	// workspace start would apply the same through the connection hook.
	waitForFile(t, sessionConfig, gatewayURL)
	if raw, _ := os.ReadFile(filepath.Join(credDir, "config.toml")); string(raw) != string(pool) {
		t.Fatalf("gateway URL leaked into the account pool:\n%s", raw)
	}
	if raw, _ := os.ReadFile(sessionConfig); !strings.Contains(string(raw), "session-model") {
		t.Fatalf("session settings lost:\n%s", raw)
	}
	if !gatewayEligible(acct) {
		t.Fatal("Codex API key account not eligible")
	}
	env := s.extensionEnv(sess, acct, []string{"NO_PROXY=localhost"})
	if envValue(env, "NO_PROXY") != "localhost,127.0.0.1" || envValue(env, envAnthropicBaseURL) != "" {
		t.Fatalf("Codex env: %v", env)
	}
	if v := s.view(sess); len(v.Extensions) != 1 {
		t.Fatalf("workspace view hides the inspecting extension: %+v", v.Extensions)
	}

	deadline := time.Now().Add(5 * time.Second)
	for s.extensions.Status("agentbox.loop-guard").State != "running" {
		if time.Now().After(deadline) {
			t.Fatal("extension did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	gw := httptest.NewServer(s.extGW.proxy)
	defer gw.Close()
	req, _ := http.NewRequest("POST", gw.URL+"/x/"+sess.ID+"/"+s.extensionMAC(sess.ID)+"/responses", strings.NewReader(`{"stream":true,"input":"x"}`))
	req.Header.Set("Authorization", "Bearer codex-key")
	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("X-Agentbox-Extensions") != "monitoring" || !strings.Contains(string(body), "response.failed") ||
		!strings.Contains(string(body), "model_output_loop") || calls.Load() != 1 {
		t.Fatalf("Codex loop not interrupted: %s calls=%d %.300s", resp.Header.Get("X-Agentbox-Extensions"), calls.Load(), body)
	}
	if events := s.extensions.Events("agentbox.loop-guard"); len(events) != 1 || events[0].Account != "codex" || events[0].Session != sess.ID {
		t.Fatalf("events: %+v", events)
	}

	// Unbinding restores the pool's real upstream for the next CLI start.
	w := extensionRequest(s, "root", "PUT", "/api/extensions/agentbox.loop-guard",
		`{"expected_revision":1,"enabled":true,"permissions":["model.request.fingerprint","model.stream.text"],"bindings":{"mode":"none"}}`,
		s.handleUpdateExtension, "agentbox.loop-guard")
	if w.Code != http.StatusOK {
		t.Fatalf("unbind: %d %s", w.Code, w.Body.String())
	}
	waitForFile(t, sessionConfig, upstream.URL+"/v1")
	if raw, _ := os.ReadFile(sessionConfig); strings.Contains(string(raw), "/x/") {
		t.Fatalf("gateway URL left behind:\n%s", raw)
	}
}

func TestExtensionGatewayDedicatedConnections(t *testing.T) {
	s, _ := extensionTestServer(t, "https://relay.example.com")
	acct, _ := s.cfg.Account("relay")
	rt, err := s.extensionTransport(acct)
	if err != nil || rt.(*http.Transport).DisableKeepAlives {
		t.Fatalf("default transport: %v", err)
	}
	put := func(body string) *httptest.ResponseRecorder {
		return extensionRequest(s, "root", "PUT", "/api/extensions/gateway", body, s.handleUpdateExtensionGateway, "")
	}
	if w := put(`{"dedicated_connections":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing revision accepted: %d", w.Code)
	}
	w := put(`{"expected_revision":0,"dedicated_connections":true}`)
	var gw extensionGatewayView
	if err := json.Unmarshal(w.Body.Bytes(), &gw); w.Code != http.StatusOK || err != nil || !gw.DedicatedConnections || gw.Revision != 1 {
		t.Fatalf("enable dedicated: %d %s", w.Code, w.Body.String())
	}
	if w := put(`{"expected_revision":0,"dedicated_connections":false}`); w.Code != http.StatusConflict {
		t.Fatalf("stale revision: %d", w.Code)
	}
	rt, err = s.extensionTransport(acct)
	tr := rt.(*http.Transport)
	if err != nil || !tr.DisableKeepAlives || tr.ForceAttemptHTTP2 || !tr.DisableCompression || tr.ResponseHeaderTimeout == 0 {
		t.Fatalf("dedicated transport not applied: %+v %v", tr, err)
	}
}
