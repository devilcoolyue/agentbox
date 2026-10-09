package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/chat"
	"agentbox/internal/config"
	"agentbox/internal/imageupdate"
	"agentbox/internal/store"
)

func discover(t *testing.T, s *Server, id string) (int, modelDiscovery, string) {
	t.Helper()
	return discoverQuery(t, s, id, "")
}

func discoverQuery(t *testing.T, s *Server, id, query string) (int, modelDiscovery, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := accessRequest("root", "POST", "/api/accounts/"+id+"/models/discover"+query, "")
	r.SetPathValue("id", id)
	s.handleAccountModelsDiscover(w, r)
	var out modelDiscovery
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Body.String()
}

func modelIDs(models []discoveredModel) string {
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return strings.Join(ids, ",")
}

func TestDiscoverModelsFromAPIKeysAndSubscriptions(t *testing.T) {
	var codexQuery, codexAccount string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/relay/v1/models" && r.Header.Get("x-api-key") == "claude-relay-key":
			// Anthropic-style paging: the second page follows last_id.
			if r.URL.Query().Get("limit") != "1000" {
				t.Errorf("claude list without a page size: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("after_id") == "" {
				fmt.Fprint(w, `{"data":[{"id":"relay-a","display_name":"Relay A","capabilities":{"effort":{"supported":true,"low":{"supported":true}}}},{"id":"org/unsupported"}],"has_more":true,"last_id":"relay-a"}`)
				return
			}
			// Anthropic ModelInfo.capabilities: native effort levels, a thinking
			// budget only, or no thinking at all.
			fmt.Fprint(w, `{"data":[{"id":"claude-opus-5-20260101"},{"id":"relay-a"},
				{"id":"claude-fable-5","capabilities":{"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":null,"max":{"supported":true}},"thinking":{"supported":true,"types":{"adaptive":{"supported":true},"enabled":{"supported":false}}}}},
				{"id":"claude-haiku-4-5","capabilities":{"effort":{"supported":false,"low":{"supported":false}},"thinking":{"supported":true,"types":{"enabled":{"supported":true}}}}},
				{"id":"claude-tiny","capabilities":{"effort":{"supported":false},"thinking":{"supported":false,"types":{}}}}],"has_more":false}`)
		case r.URL.Path == "/openai/v1/models" && r.Header.Get("Authorization") == "Bearer codex-relay-key":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"deepseek-v4","object":"model"},{"id":"deepseek-v4-flash","object":"model"}]}`)
		case r.URL.Path == "/backend-api/codex/models" && r.Header.Get("Authorization") == "Bearer chatgpt-access":
			codexQuery, codexAccount = r.URL.RawQuery, r.Header.Get("ChatGPT-Account-Id")
			fmt.Fprint(w, `{"models":[
				{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list","minimal_client_version":"0.98.0","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"bogus"}]},
				{"slug":"internal-only","display_name":"Hidden","visibility":"hide"},
				{"slug":"needs-newer-cli","visibility":"list","minimal_client_version":"9.0.0"},
				{"slug":"gpt-5.5-mini","visibility":"list","supported_reasoning_levels":[]}]}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"unauthorized"}`)
		}
	}))
	defer up.Close()
	defer func(previous string) { codexModelsURL = previous }(codexModelsURL)
	codexModelsURL = up.URL + "/backend-api/codex/models"

	s, _ := newTestServer(t)
	s.store.CreateUser(store.User{Name: "root", Role: store.RoleAdmin})
	codexKeyDir, codexSubDir := t.TempDir(), t.TempDir()
	writeFile := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(codexKeyDir, "auth.json", `{"OPENAI_API_KEY":"codex-relay-key"}`)
	writeFile(codexKeyDir, "config.toml", "model_provider = \"agentbox\"\n[model_providers.agentbox]\nbase_url = \""+up.URL+"/openai/v1\"\nwire_api = \"chat\"\n")
	writeFile(codexSubDir, "auth.json", `{"auth_mode":"chatgpt","tokens":{"access_token":"chatgpt-access","refresh_token":"r","id_token":"a.b.c","account_id":"workspace-1"}}`)
	s.cfg.Accounts = []config.Account{
		{ID: "claude-relay", Type: config.AgentClaude, Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "claude-relay-key", "ANTHROPIC_BASE_URL": up.URL + "/relay"},
			ModelReasoning: map[string]config.ReasoningCapability{"relay-a": {Support: "unsupported"}}},
		{ID: "codex-relay", Type: config.AgentCodex, CredentialsDir: codexKeyDir},
		{ID: "codex-sub", Type: config.AgentCodex, CredentialsDir: codexSubDir},
		{ID: "codex-empty", Type: config.AgentCodex, CredentialsDir: t.TempDir()},
	}
	s.cfg.Models = map[string][]config.ModelOption{config.AgentClaude: {{ID: "claude-opus-5", Label: "Opus 5", Reasoning: &config.ReasoningCapability{Support: "supported", Control: "effort", Levels: []string{"low", "max"}}}}}

	code, got, body := discover(t, s, "claude-relay")
	if code != 200 || got.Source != "api" || modelIDs(got.Models) != "relay-a,claude-opus-5-20260101,claude-fable-5,claude-haiku-4-5,claude-tiny" || strings.Join(got.Skipped, ",") != "org/unsupported" {
		t.Fatalf("claude relay: %d %s", code, body)
	}
	// The account's own setting outranks what the upstream reports.
	if got.Models[0].Label != "Relay A" || got.Models[0].ReasoningSource != "account" || got.Models[0].Reasoning.Support != "unsupported" {
		t.Fatalf("account override not suggested: %+v", got.Models[0])
	}
	// A dated snapshot reuses the configured capability of the same model.
	if m := got.Models[1]; m.ReasoningSource != "global" || strings.Join(m.Reasoning.Levels, ",") != "low,max" {
		t.Fatalf("global capability not suggested: %+v", m)
	}
	for i, want := range []string{"supported/effort/low,medium,high,max", "supported/budget/low,medium,high,xhigh", "unsupported//"} {
		m := got.Models[2+i]
		if m.Reasoning == nil || m.ReasoningSource != "upstream" || m.Reasoning.Support+"/"+m.Reasoning.Control+"/"+strings.Join(m.Reasoning.Levels, ",") != want {
			t.Fatalf("%s capability = %+v (%s), want %s", m.ID, m.Reasoning, m.ReasoningSource, want)
		}
	}

	code, got, body = discover(t, s, "codex-relay")
	if code != 200 || modelIDs(got.Models) != "deepseek-v4,deepseek-v4-flash" || got.Endpoint != up.URL+"/openai/v1/models" || got.Models[0].Reasoning != nil {
		t.Fatalf("codex relay: %d %s", code, body)
	}
	if strings.Contains(body, "codex-relay-key") {
		t.Fatal("API key echoed")
	}

	code, got, body = discover(t, s, "codex-sub")
	if code != 200 || got.Source != "codex_subscription" || modelIDs(got.Models) != "gpt-5.5,gpt-5.5-mini" {
		t.Fatalf("codex subscription: %d %s", code, body)
	}
	if codexQuery != "client_version="+fallbackCodexVersion || codexAccount != "workspace-1" {
		t.Fatalf("catalog request = %q / %q", codexQuery, codexAccount)
	}
	if m := got.Models[0]; m.Label != "GPT-5.5" || m.ReasoningSource != "upstream" || strings.Join(m.Reasoning.Levels, ",") != "low,high" {
		t.Fatalf("upstream efforts not used: %+v", m)
	}
	// No listed effort is unknown, not "unsupported".
	if got.Models[1].Reasoning != nil {
		t.Fatalf("empty effort list became a capability: %+v", got.Models[1])
	}
	if strings.Contains(body, "chatgpt-access") {
		t.Fatal("access token echoed")
	}

	if code, _, body = discover(t, s, "codex-empty"); code != http.StatusBadGateway || !strings.Contains(body, "尚未登录") {
		t.Fatalf("signed-out account: %d %s", code, body)
	}
}

func TestDiscoverUsesTheOfficialCatalogForRelays(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/plain/v1/models" {
			// An OpenAI-style relay list: IDs only.
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-6.1-sol"},{"id":"gpt-5.4"},{"id":"deepseek-v4"}]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"no such route"}`)
	}))
	defer up.Close()
	reads := 0
	catalog := `{"version":1,
	 "claude":{"models":[
	  {"value":"opus","resolvedModel":"claude-opus-5-5","description":"Opus 5.5 · Best","supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max"]},
	  {"value":"haiku","resolvedModel":"claude-haiku-4-5-20251001","description":"Haiku 4.5 · Fast"},
	  {"value":"next","resolvedModel":"claude-next-6","description":"Next 6 · New","supportsEffort":true,"supportedEffortLevels":["low","high"]}]},
	 "codex":{"models":[{"model":"gpt-6.1-sol","displayName":"GPT-6.1-Sol","efforts":["low","medium","high","xhigh","max","ultra"]}]}}`
	identityErr := error(nil)
	defer func(i, r any) {
		agentImageIdentity = i.(func(context.Context, *Server) (imageupdate.Image, error))
		readCLICatalog = r.(func(context.Context, *Server, string) ([]byte, error))
	}(agentImageIdentity, readCLICatalog)
	agentImageIdentity = func(context.Context, *Server) (imageupdate.Image, error) {
		return imageupdate.Image{ID: "sha256:" + strings.Repeat("c", 64), Claude: "2.1.295", Codex: "0.162.0"}, identityErr
	}
	readCLICatalog = func(context.Context, *Server, string) ([]byte, error) { reads++; return []byte(catalog), nil }

	s, _ := newTestServer(t)
	s.store.CreateUser(store.User{Name: "root", Role: store.RoleAdmin})
	keyDir, subDir := t.TempDir(), t.TempDir()
	for dir, files := range map[string]map[string]string{
		keyDir: {"auth.json": `{"OPENAI_API_KEY":"plain-key"}`, "config.toml": "model_provider = \"relay\"\n[model_providers.relay]\nbase_url = \"" + up.URL + "/plain\"\n"},
		subDir: {"auth.json": `{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r","id_token":"a.b.c"}}`},
	} {
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.cfg.Accounts = []config.Account{
		{ID: "claude-nolist", Type: config.AgentClaude, Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "relay-key", "ANTHROPIC_BASE_URL": up.URL + "/nolist"},
			ModelReasoning: map[string]config.ReasoningCapability{"claude-next-6": {Support: "unknown"}}},
		{ID: "codex-plain", Type: config.AgentCodex, CredentialsDir: keyDir},
		{ID: "codex-sub", Type: config.AgentCodex, CredentialsDir: subDir},
	}

	// A relay without a model list gets the official catalog instead of an error.
	code, got, body := discover(t, s, "claude-nolist")
	if code != 200 || got.Source != "official" || !strings.Contains(got.UpstreamError, "HTTP 404") || got.Official == nil || got.Official.Kind != "cli" || got.Official.CLIVersion != "2.1.295" {
		t.Fatalf("fallback: %d %s", code, body)
	}
	byID := map[string]discoveredModel{}
	for _, m := range got.Models {
		byID[m.ID] = m
	}
	if m := byID["claude-opus-5-5"]; !m.Recommended || m.Label != "Claude Opus 5.5" || m.ReasoningSource != "official" || len(m.Reasoning.Levels) != 5 {
		t.Fatalf("CLI model: %+v", m)
	}
	// The CLI reports no effort for Haiku 4.5; the snapshot knows its budget.
	if m := byID["claude-haiku-4-5-20251001"]; !m.Recommended || m.Reasoning == nil || m.Reasoning.Control != "budget" {
		t.Fatalf("snapshot fill: %+v", m)
	}
	// Older snapshot models are listed but not preselected; account settings win.
	if m := byID["claude-opus-4-6"]; m.Recommended || m.Reasoning == nil || m.ReasoningSource != "official" {
		t.Fatalf("snapshot model: %+v", m)
	}
	if m := byID["claude-next-6"]; m.ReasoningSource != "account" || m.Reasoning.Support != "unknown" {
		t.Fatalf("account setting must outrank the catalog: %+v", m)
	}

	// A plain relay list keeps its IDs; the catalog adds labels and reasoning.
	code, got, body = discover(t, s, "codex-plain")
	if code != 200 || got.Source != "api" || modelIDs(got.Models) != "gpt-6.1-sol,gpt-5.4,deepseek-v4" || got.Official == nil || got.Official.CLIVersion != "0.162.0" {
		t.Fatalf("plain relay: %d %s", code, body)
	}
	if m := got.Models[0]; m.Label != "GPT-6.1-Sol" || m.ReasoningSource != "official" || strings.Join(m.Reasoning.Levels, ",") != "low,medium,high,xhigh,max,ultra" {
		t.Fatalf("CLI catalog fill: %+v", m)
	}
	if m := got.Models[1]; m.Label != "GPT-5.4" || m.ReasoningSource != "official" {
		t.Fatalf("snapshot fill: %+v", m)
	}
	if m := got.Models[2]; m.Reasoning != nil || m.Label != "deepseek-v4" {
		t.Fatalf("unknown model must stay unknown: %+v", m)
	}

	// The official listing needs no sign-in or network; a failed subscription
	// read is still an error rather than a silent catalog.
	code, got, body = discoverQuery(t, s, "codex-sub", "?source=official")
	if code != 200 || got.Source != "official" || got.UpstreamError != "" || got.Models[0].ID != "gpt-6.1-sol" || !got.Models[0].Recommended {
		t.Fatalf("explicit official: %d %s", code, body)
	}
	if reads != 1 {
		t.Fatalf("the image's catalog is read once per image, got %d", reads)
	}

	// Without a readable image, the compiled-in snapshot answers.
	s.cliCatalog = cliCatalogCache{}
	identityErr = errors.New("no docker")
	code, got, body = discoverQuery(t, s, "codex-sub", "?source=official")
	if code != 200 || got.Official == nil || got.Official.Kind != "builtin" || !got.Official.Unavailable || got.Official.VerifiedAt == "" || len(got.Models) == 0 || strings.Contains(body, "no docker") {
		t.Fatalf("snapshot only: %d %s", code, body)
	}
	if strings.Contains(body, "plain-key") || strings.Contains(body, "relay-key") {
		t.Fatal("key echoed")
	}
}

func TestCLICatalogRetriesOnlyAfterAFailure(t *testing.T) {
	defer func(i, r any) {
		agentImageIdentity = i.(func(context.Context, *Server) (imageupdate.Image, error))
		readCLICatalog = r.(func(context.Context, *Server, string) ([]byte, error))
	}(agentImageIdentity, readCLICatalog)
	image := "sha256:" + strings.Repeat("d", 64)
	agentImageIdentity = func(context.Context, *Server) (imageupdate.Image, error) { return imageupdate.Image{ID: image}, nil }
	reads := 0
	output := `{"version":1,"claude":{"error":"timeout"},"codex":{"models":[{"model":"gpt-5.5","efforts":["low"]}]}}`
	readCLICatalog = func(context.Context, *Server, string) ([]byte, error) { reads++; return []byte(output), nil }
	s, _ := newTestServer(t)
	ctx := t.Context()
	models, _, _ := s.cliModels(ctx)
	if len(models[config.AgentCodex]) != 1 || models[config.AgentClaude] != nil {
		t.Fatalf("partial read: %+v", models)
	}
	s.cliModels(ctx)
	if reads != 1 {
		t.Fatal("a partial read is retried only after a pause")
	}
	s.cliCatalog.at = s.cliCatalog.at.Add(-cliCatalogRetry)
	output = `{"version":1,"claude":{"models":[{"resolvedModel":"claude-opus-5-5"}]},"codex":{"models":[{"model":"gpt-5.5"}]}}`
	s.cliModels(ctx)
	s.cliCatalog.at = s.cliCatalog.at.Add(-time.Hour)
	if models, _, _ = s.cliModels(ctx); reads != 2 || len(models[config.AgentClaude]) != 1 {
		t.Fatalf("complete reads are kept per image: reads=%d %+v", reads, models)
	}
	image = "sha256:" + strings.Repeat("e", 64)
	if s.cliModels(ctx); reads != 3 {
		t.Fatal("a new image is read again")
	}
}

func TestAccountModelListLimitsWorkspaceChoices(t *testing.T) {
	s, sess := accessTestServer(t)
	sess.AccountID, sess.DefaultModel = "shared", "claude-opus-5"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	patch := func(body string) (int, string) {
		w := httptest.NewRecorder()
		r := accessRequest("root", "PATCH", "/api/accounts/shared", body)
		r.SetPathValue("id", "shared")
		s.handleAccountPatch(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := patch(`{"models":[{"id":"relay-a","label":"Relay A","reasoning":{"support":"supported","control":"effort","levels":["low","high"]}},{"id":"relay-b"}]}`); code != 200 || !strings.Contains(body, `"default_model":"relay-a"`) {
		t.Fatalf("patch: %d %s", code, body)
	}
	if code, _ := patch(`{"default_model":"relay-z"}`); code != http.StatusBadRequest {
		t.Fatal("default outside the list accepted")
	}

	w := httptest.NewRecorder()
	s.handleSessionModels(w, accessRequest("alice", "GET", "/api/sessions/s1/models", ""), sess)
	var view sessionModelsView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Restricted || view.DefaultModel != "relay-a" || len(view.Models) != 2 || view.Models[0].Label != "Relay A" {
		t.Fatalf("view = %+v", view)
	}
	if r := view.Models[0].Reasoning; r == nil || r.Support != "supported" || strings.Join(r.Levels, ",") != "low,high" {
		t.Fatalf("account capability missing: %+v", r)
	}
	if got := s.view(sess); got.DefaultModel != "relay-a" {
		t.Fatalf("session view reports a model the account does not offer: %q", got.DefaultModel)
	}

	runtime := chatRuntime{room: &chatRoom{srv: s, sessID: sess.ID}}
	if _, err := runtime.Options(t.Context(), sess, "gpt-5.5", "", "", false); !errors.Is(err, chat.ErrOptions) {
		t.Fatalf("model outside the list accepted: %v", err)
	}
	// A turn on the stale workspace default runs on the account default.
	options, err := runtime.Options(t.Context(), sess, sess.DefaultModel, "", "", false)
	if err != nil || options.Model != "relay-a" {
		t.Fatalf("stale default not resolved: %+v %v", options, err)
	}
	if _, err := runtime.Options(t.Context(), sess, "relay-a", "high", "effort", false); err != nil {
		t.Fatalf("listed model with a supported effort rejected: %v", err)
	}
	if _, err := runtime.Options(t.Context(), sess, "relay-a", "max", "effort", false); !errors.Is(err, chat.ErrOptions) {
		t.Fatal("effort outside the model's levels accepted")
	}

	// Hiding keeps the model on the account: still listed (flagged) for the
	// page, still accepted for a turn. Hiding the default moves the default.
	if code, body := patch(`{"models":[{"id":"relay-a","label":"Relay A","hidden":true},{"id":"relay-b"}]}`); code != 200 || !strings.Contains(body, `"default_model":"relay-b"`) {
		t.Fatalf("hide default: %d %s", code, body)
	}
	if code, _ := patch(`{"models":[{"id":"relay-a","hidden":true}]}`); code != http.StatusBadRequest {
		t.Fatal("a list with nothing shown accepted")
	}
	w = httptest.NewRecorder()
	s.handleSessionModels(w, accessRequest("alice", "GET", "/api/sessions/s1/models", ""), sess)
	view = sessionModelsView{}
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if len(view.Models) != 2 || !view.Models[0].Hidden || view.Models[1].Hidden || view.DefaultModel != "relay-b" {
		t.Fatalf("hidden model view = %+v", view)
	}
	if _, err := runtime.Options(t.Context(), sess, "relay-a", "", "", false); err != nil {
		t.Fatalf("hidden model rejected: %v", err)
	}

	// [] returns the account to the global list.
	if code, body := patch(`{"models":[]}`); code != 200 || strings.Contains(body, "default_model") {
		t.Fatalf("clear: %d %s", code, body)
	}
	w = httptest.NewRecorder()
	s.handleSessionModels(w, accessRequest("alice", "GET", "/api/sessions/s1/models", ""), sess)
	view = sessionModelsView{}
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if view.Restricted || view.DefaultModel != "claude-opus-5" {
		t.Fatalf("cleared list still restricts: %+v", view)
	}

	// Regular users never receive the account's list through /accounts.
	if code, _ := patch(`{"models":[{"id":"relay-a"}]}`); code != 200 {
		t.Fatal("re-patch failed")
	}
	w = httptest.NewRecorder()
	s.handleAccounts(w, accessRequest("alice", "GET", "/api/accounts", ""))
	if strings.Contains(w.Body.String(), `"models"`) || !strings.Contains(w.Body.String(), `"default_model":"relay-a"`) {
		t.Fatalf("user account view: %s", w.Body.String())
	}
}

func TestSessionModelsReportTheAccountsClientEffort(t *testing.T) {
	dir := t.TempDir()
	toml := "model_reasoning_effort = \"low\"\nprofile = \"work\"\n[profiles.work]\nmodel_reasoning_effort = \"xhigh\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		acct config.Account
		want string
	}{
		// The active profile wins, as in the Codex CLI.
		{config.Account{Type: config.AgentCodex, CredentialsDir: dir}, "xhigh"},
		{config.Account{Type: config.AgentCodex, CredentialsDir: t.TempDir()}, ""},
		{config.Account{Type: config.AgentClaude, Env: map[string]string{"CLAUDE_CODE_EFFORT_LEVEL": "max"}}, "max"},
		// Not a native level: nothing is offered as the default.
		{config.Account{Type: config.AgentClaude, Env: map[string]string{"CLAUDE_CODE_EFFORT_LEVEL": "auto"}}, ""},
	} {
		if got := accountClientEffort(c.acct); got != c.want {
			t.Errorf("%s %v: client effort = %q, want %q", c.acct.Type, c.acct.Env, got, c.want)
		}
	}
}
