package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/credentials"
	"agentbox/internal/imageupdate"
	"agentbox/internal/modelcatalog"
)

// Variables rather than constants so tests can point them at a fake upstream.
var (
	anthropicAPIBase = "https://api.anthropic.com"
	// The catalog the Codex CLI itself reads for ChatGPT sign-ins; it carries
	// each model's supported reasoning efforts.
	codexModelsURL = "https://chatgpt.com/backend-api/codex/models"
)

// fallbackCodexVersion is the image default when the configured image's label
// cannot be read; the catalog hides models newer CLIs need either way.
const fallbackCodexVersion = "0.145.0"

var modelDateSuffix = regexp.MustCompile(`-\d{8}$`)

type discoveredModel struct {
	ID        string                      `json:"id"`
	Label     string                      `json:"label"`
	Reasoning *config.ReasoningCapability `json:"reasoning,omitempty"`
	// upstream: reported by the provider; account / global: the capability
	// already configured here for the same model; official: the official
	// catalog's entry for the same model ID. Empty: unknown.
	ReasoningSource string `json:"reasoning_source,omitempty"`
	// Official listings only: the CLI's current model, preselected.
	Recommended bool `json:"recommended,omitempty"`
}

type modelDiscovery struct {
	Source    string            `json:"source"` // api | claude_subscription | codex_subscription | official
	Endpoint  string            `json:"endpoint"`
	LatencyMS int64             `json:"latency_ms"`
	Models    []discoveredModel `json:"models"`
	// IDs the CLIs cannot be given safely (characters outside the model ID
	// rule). They are reported, not silently dropped.
	Skipped []string `json:"skipped,omitempty"`
	// The relay or API endpoint could not list models; Models is the
	// official catalog instead.
	UpstreamError string `json:"upstream_error,omitempty"`
	// Set when official data listed the models or filled labels/reasoning.
	Official *officialSource `json:"official,omitempty"`
}

// handleAccountModelsDiscover lists what the account's upstream offers, or
// with ?source=official the official catalog (no account or network needed).
// It is read-only: nothing is saved until the administrator submits a selection.
func (s *Server) handleAccountModelsDiscover(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.cfg.Account(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if acct.Type != config.AgentClaude && acct.Type != config.AgentCodex {
		writeErr(w, http.StatusBadGateway, "该账号类型不支持读取模型列表")
		return
	}
	var out modelDiscovery
	var official []modelcatalog.Model
	if r.URL.Query().Get("source") == "official" {
		out, official = s.officialDiscovery(r.Context(), acct.Type)
	} else {
		// Requests use the account's own egress, like the CLI in its workspaces.
		client, err := s.acctClient(acct, 20*time.Second)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		defer client.CloseIdleConnections()
		if acct.Type == config.AgentClaude {
			out, err = s.discoverClaudeModels(r.Context(), client, acct)
		} else {
			out, err = s.discoverCodexModels(r.Context(), client, acct)
		}
		if err != nil {
			// Many relays have no model list. A failed subscription read is a
			// sign-in problem the official catalog would only hide.
			if !discoversViaAPI(acct) {
				writeErr(w, http.StatusBadGateway, err.Error())
				return
			}
			out, official = s.officialDiscovery(r.Context(), acct.Type)
			out.UpstreamError = err.Error()
		}
	}
	for i := range out.Models {
		m := &out.Models[i]
		// What the administrator already set for this account outranks the
		// provider's report (a relay may not pass the option on); the global
		// list, then the official catalog, only describe models nothing
		// else does.
		if r := accountReasoning(acct, m.ID); r != nil {
			m.Reasoning, m.ReasoningSource = r, "account"
		} else if m.Reasoning == nil {
			m.Reasoning, m.ReasoningSource = s.globalReasoning(acct.Type, m.ID)
		}
		if m.Reasoning != nil && m.Label != m.ID {
			continue
		}
		// Relay lists rarely carry reasoning or display names. Read the
		// official catalog only then: it may start a sandbox container.
		if out.Official == nil {
			var src officialSource
			official, src = s.officialModels(r.Context(), acct.Type)
			out.Official = &src
		}
		if o, ok := modelcatalog.Lookup(official, m.ID); ok {
			if m.Label == m.ID {
				m.Label = o.Label
			}
			if m.Reasoning == nil && o.Reasoning != nil {
				m.Reasoning, m.ReasoningSource = config.CloneReasoning(o.Reasoning), "official"
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// officialDiscovery lists the official catalog in the discovery shape. The
// reasoning is filled by the caller so account settings still come first.
func (s *Server) officialDiscovery(ctx context.Context, agentType string) (modelDiscovery, []modelcatalog.Model) {
	start := time.Now()
	official, src := s.officialModels(ctx, agentType)
	out := modelDiscovery{Source: "official", Models: []discoveredModel{}, Official: &src}
	for _, m := range official {
		out.Models = append(out.Models, discoveredModel{ID: m.ID, Label: m.Label, Recommended: m.Current})
	}
	out.LatencyMS = time.Since(start).Milliseconds()
	return out, official
}

// discoversViaAPI: the account lists models with an API key (official API or
// relay), not with a subscription sign-in.
func discoversViaAPI(acct config.Account) bool {
	switch acct.Type {
	case config.AgentClaude:
		_, token := claudeRelay(acct)
		return token != ""
	case config.AgentCodex:
		return credentials.CodexAuthMode(acct) == "apikey"
	}
	return false
}

func accountReasoning(acct config.Account, id string) *config.ReasoningCapability {
	if r, ok := acct.ModelReasoning[id]; ok {
		return config.CloneReasoning(&r)
	}
	for _, m := range acct.Models {
		if m.ID == id && m.Reasoning != nil {
			return config.CloneReasoning(m.Reasoning)
		}
	}
	return nil
}

// globalReasoning reuses the global model list: exact ID, then without a
// -YYYYMMDD snapshot suffix, which names the same model.
func (s *Server) globalReasoning(agentType, id string) (*config.ReasoningCapability, string) {
	global := s.cfg.GetModels()[agentType]
	for _, key := range []string{id, modelDateSuffix.ReplaceAllString(id, "")} {
		for _, m := range global {
			if m.ID == key && m.Reasoning != nil {
				return config.CloneReasoning(m.Reasoning), "global"
			}
		}
	}
	return nil, ""
}

func (s *Server) discoverClaudeModels(ctx context.Context, client *http.Client, acct config.Account) (modelDiscovery, error) {
	if base, token := claudeRelay(acct); token != "" {
		if base == "" {
			base = anthropicAPIBase
		}
		return listProviderModels(ctx, client, acct.Type, strings.TrimRight(base, "/"), func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("x-api-key", token)
			req.Header.Set("anthropic-version", "2023-06-01")
		})
	}
	if acct.CredentialsDir == "" {
		return modelDiscovery{}, fmt.Errorf("账号未配置凭证目录")
	}
	cred, err := credentials.ReadClaude(acct)
	if err != nil {
		return modelDiscovery{}, fmt.Errorf("账号尚未完成订阅登录")
	}
	if cred.Expiring() {
		if cred, err = s.ensureClaudeCred(ctx, acct, false); err != nil {
			return modelDiscovery{}, err
		}
	}
	list := func(token string) (modelDiscovery, error) {
		return listProviderModels(ctx, client, acct.Type, anthropicAPIBase+"/v1", func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("anthropic-beta", claudeOAuthBeta)
			req.Header.Set("anthropic-version", "2023-06-01")
		})
	}
	out, err := list(cred.AccessToken)
	// expiresAt is not the only truth: renew once on an authentication failure.
	var status *upstreamStatus
	if err != nil && asUpstreamStatus(err, &status) && status.code == http.StatusUnauthorized {
		if cred, err = s.ensureClaudeCred(ctx, acct, true); err != nil {
			return modelDiscovery{}, err
		}
		out, err = list(cred.AccessToken)
	}
	out.Source = "claude_subscription"
	return out, err
}

func (s *Server) discoverCodexModels(ctx context.Context, client *http.Client, acct config.Account) (modelDiscovery, error) {
	switch credentials.CodexAuthMode(acct) {
	case "apikey":
		key := credentials.SavedAPIKey(acct.CredentialsDir)
		base, _ := credentials.ReadCodexProvider(acct.CredentialsDir)
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return listProviderModels(ctx, client, acct.Type, strings.TrimRight(base, "/"), func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+key)
		})
	case "oauth":
	default:
		return modelDiscovery{}, fmt.Errorf("账号尚未登录：请先完成订阅授权或保存 API Key")
	}
	token, accountID, err := s.credentialService().CodexAccess(ctx, acct)
	if err != nil {
		return modelDiscovery{}, err
	}
	version := s.codexCLIVersion(ctx)
	endpoint := codexModelsURL + "?" + url.Values{"client_version": {version}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return modelDiscovery{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("User-Agent", "codex_cli_rs/"+version)
	start := time.Now()
	raw, err := doModelsRequest(client, req)
	if err != nil {
		var status *upstreamStatus
		if asUpstreamStatus(err, &status) && status.code == http.StatusUnauthorized {
			return modelDiscovery{}, fmt.Errorf("订阅登录已过期：在使用该账号的空间里发一轮对话让 Codex 续期，或重新授权后再试")
		}
		return modelDiscovery{}, err
	}
	var body struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
			MinVersion  string `json:"minimal_client_version"`
			Levels      []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Models == nil {
		return modelDiscovery{}, fmt.Errorf("订阅模型目录的响应格式无法识别")
	}
	out := modelDiscovery{Source: "codex_subscription", Endpoint: codexModelsURL, LatencyMS: time.Since(start).Milliseconds(), Models: []discoveredModel{}}
	seen := map[string]bool{}
	for _, m := range body.Models {
		// Only what the CLI's own picker lists, and only models this CLI runs.
		if m.Visibility != "" && m.Visibility != "list" || versionBelow(version, m.MinVersion) || seen[m.Slug] {
			continue
		}
		seen[m.Slug] = true
		if !config.ValidModelID(m.Slug) {
			out.Skipped = append(out.Skipped, m.Slug)
			continue
		}
		entry := discoveredModel{ID: m.Slug, Label: firstNonEmpty(strings.TrimSpace(m.DisplayName), m.Slug)}
		levels := []string{}
		for _, l := range m.Levels {
			if slices.Contains(config.ReasoningLevels(config.AgentCodex, "effort"), l.Effort) && !slices.Contains(levels, l.Effort) {
				levels = append(levels, l.Effort)
			}
		}
		// No listed effort is not proof the model rejects one; leave it unknown.
		if len(levels) > 0 {
			entry.Reasoning = &config.ReasoningCapability{Support: "supported", Control: "effort", Levels: levels}
			entry.ReasoningSource = "upstream"
		}
		out.Models = append(out.Models, entry)
	}
	return out, nil
}

// codexCLIVersion reads the version label of the configured agent image.
func (s *Server) codexCLIVersion(ctx context.Context) string {
	if s.dock == nil {
		return fallbackCodexVersion
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	img, err := s.dock.InspectCLIImage(ctx, s.cfg.GetAgentImage())
	if err != nil || !imageupdate.ValidVersion(img.Codex) {
		return fallbackCodexVersion
	}
	return img.Codex
}

// versionBelow reports a < b for dotted numeric versions; unparseable input
// never hides a model.
func versionBelow(a, b string) bool {
	if b == "" {
		return false
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		x, y := 0, 0
		var err error
		if i < len(pa) {
			if x, err = strconv.Atoi(pa[i]); err != nil {
				return false
			}
		}
		if i < len(pb) {
			if y, err = strconv.Atoi(strings.SplitN(pb[i], "-", 2)[0]); err != nil {
				return false
			}
		}
		if x != y {
			return x < y
		}
	}
	return false
}

type upstreamStatus struct {
	code int
	msg  string
}

func (e *upstreamStatus) Error() string { return e.msg }

func asUpstreamStatus(err error, target **upstreamStatus) bool {
	e, ok := err.(*upstreamStatus)
	if ok {
		*target = e
	}
	return ok
}

func doModelsRequest(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s → %v", req.URL.Host+req.URL.Path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &upstreamStatus{code: resp.StatusCode, msg: fmt.Sprintf("%s → HTTP %d: %s", req.URL.Host+req.URL.Path, resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200))}
	}
	return raw, nil
}

type capabilitySupport struct {
	Supported bool `json:"supported"`
}

// claudeCapabilities is the reasoning part of an Anthropic ModelInfo
// (GET /v1/models). OpenAI-style lists carry nothing comparable.
type claudeCapabilities struct {
	Effort *struct {
		Supported bool               `json:"supported"`
		Low       *capabilitySupport `json:"low"`
		Medium    *capabilitySupport `json:"medium"`
		High      *capabilitySupport `json:"high"`
		XHigh     *capabilitySupport `json:"xhigh"`
		Max       *capabilitySupport `json:"max"`
	} `json:"effort"`
	Thinking *struct {
		Supported bool `json:"supported"`
		Types     struct {
			Enabled *capabilitySupport `json:"enabled"`
		} `json:"types"`
	} `json:"thinking"`
}

// reasoning maps the report onto the CLI's controls: native --effort with the
// levels the model accepts; otherwise a thinking budget when extended thinking
// with budget_tokens is accepted. A relay that drops the field stays unknown.
func (c *claudeCapabilities) reasoning() *config.ReasoningCapability {
	if c == nil {
		return nil
	}
	if e := c.Effort; e != nil && e.Supported {
		var levels []string
		for _, l := range []struct {
			name string
			flag *capabilitySupport
		}{{"low", e.Low}, {"medium", e.Medium}, {"high", e.High}, {"xhigh", e.XHigh}, {"max", e.Max}} {
			if l.flag != nil && l.flag.Supported {
				levels = append(levels, l.name)
			}
		}
		if len(levels) > 0 {
			return &config.ReasoningCapability{Support: "supported", Control: "effort", Levels: levels}
		}
	}
	t := c.Thinking
	if t == nil {
		return nil
	}
	if t.Supported && t.Types.Enabled != nil && t.Types.Enabled.Supported {
		return &config.ReasoningCapability{Support: "supported", Control: "budget", Levels: config.ReasoningLevels(config.AgentClaude, "budget")}
	}
	if !t.Supported && (c.Effort == nil || !c.Effort.Supported) {
		return &config.ReasoningCapability{Support: "unsupported"}
	}
	return nil
}

// listProviderModels reads an OpenAI- or Anthropic-style model list. Relay base
// URLs come with or without /v1, so both paths are tried. Anthropic pages with
// has_more / last_id; OpenAI-style lists ignore the extra query parameters.
func listProviderModels(ctx context.Context, client *http.Client, agentType, baseURL string, auth func(*http.Request)) (modelDiscovery, error) {
	if err := validateBaseURL(baseURL); err != nil {
		return modelDiscovery{}, err
	}
	candidates := []string{baseURL + "/models"}
	if !strings.HasSuffix(baseURL, "/v1") {
		candidates = []string{baseURL + "/v1/models", baseURL + "/models"}
	}
	var lastErr error
	for _, endpoint := range candidates {
		start := time.Now()
		out := modelDiscovery{Source: "api", Endpoint: endpoint, Models: []discoveredModel{}}
		seen := map[string]bool{}
		after := ""
		var err error
		for page := 0; page < 20; page++ {
			q := url.Values{}
			if agentType == config.AgentClaude {
				q.Set("limit", "1000")
				if after != "" {
					q.Set("after_id", after)
				}
			}
			target := endpoint
			if len(q) > 0 {
				target += "?" + q.Encode()
			}
			var req *http.Request
			if req, err = http.NewRequestWithContext(ctx, http.MethodGet, target, nil); err != nil {
				break
			}
			auth(req)
			var raw []byte
			if raw, err = doModelsRequest(client, req); err != nil {
				break
			}
			var body struct {
				Data []struct {
					ID           string              `json:"id"`
					DisplayName  string              `json:"display_name"`
					Capabilities *claudeCapabilities `json:"capabilities"`
				} `json:"data"`
				HasMore bool   `json:"has_more"`
				LastID  string `json:"last_id"`
			}
			if json.Unmarshal(raw, &body) != nil || body.Data == nil {
				err = fmt.Errorf("%s → 响应不是模型列表 JSON", endpoint)
				break
			}
			for _, m := range body.Data {
				id := strings.TrimSpace(m.ID)
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				if !config.ValidModelID(id) {
					out.Skipped = append(out.Skipped, id)
					continue
				}
				entry := discoveredModel{ID: id, Label: firstNonEmpty(strings.TrimSpace(m.DisplayName), id)}
				if agentType == config.AgentClaude {
					if entry.Reasoning = m.Capabilities.reasoning(); entry.Reasoning != nil {
						entry.ReasoningSource = "upstream"
					}
				}
				out.Models = append(out.Models, entry)
			}
			if !body.HasMore || body.LastID == "" || body.LastID == after {
				break
			}
			after = body.LastID
		}
		if err == nil {
			out.LatencyMS = time.Since(start).Milliseconds()
			return out, nil
		}
		lastErr = err
	}
	return modelDiscovery{}, lastErr
}
