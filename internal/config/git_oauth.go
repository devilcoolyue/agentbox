package config

import (
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"

	"agentbox/internal/gitaccess"
)

var ErrGitOAuthConflict = errors.New("OAuth 应用已变化，请刷新后重试")

type GitOAuthApp struct {
	Network     gitaccess.NetworkPolicy `json:"network,omitempty"`
	ID          string                  `json:"id"`
	Label       string                  `json:"label"`
	Provider    string                  `json:"provider"`
	BaseURL     string                  `json:"base_url"`
	ClientID    string                  `json:"client_id"`
	Secret      []byte                  `json:"encrypted_secret"`
	RedirectURL string                  `json:"redirect_url"`
	Enabled     bool                    `json:"enabled"`
	Revision    int64                   `json:"revision"`
}

func (a GitOAuthApp) AssociatedData() []byte {
	parts := []string{"git-oauth-app-v1", a.ID, a.Provider, a.BaseURL, a.ClientID, a.RedirectURL}
	if a.Network.Configured() {
		parts = append(parts, a.Network.Identity())
	}
	raw, _ := json.Marshal(parts)
	return raw
}
func (c *Config) GitOAuthAppList() []GitOAuthApp {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneGitOAuthApps(c.GitOAuthApps)
}
func cloneGitOAuthApps(apps []GitOAuthApp) []GitOAuthApp {
	out := slices.Clone(apps)
	for i := range out {
		out[i].Secret = slices.Clone(out[i].Secret)
	}
	return out
}
func (c *Config) GitOAuthApp(id string) (GitOAuthApp, bool) {
	for _, a := range c.GitOAuthAppList() {
		if a.ID == id {
			return a, true
		}
	}
	return GitOAuthApp{}, false
}
func (c *Config) validateGitOAuthApps() error {
	if len(c.GitOAuthApps) > 32 {
		return errors.New("OAuth 应用最多 32 个")
	}
	seen := map[string]bool{}
	for _, a := range c.GitOAuthApps {
		if err := a.Network.Validate(); err != nil {
			return err
		}
		if !accountIDRe.MatchString(a.ID) || seen[a.ID] || a.Label == "" || len(a.Label) > 128 || a.ClientID == "" || len(a.ClientID) > 256 || len(a.Secret) < 29 || a.Revision < 1 {
			return errors.New("Git OAuth 应用信息无效")
		}
		seen[a.ID] = true
		if a.Provider != "github" && a.Provider != "gitlab" {
			return errors.New("OAuth 仅支持 GitHub 和 GitLab")
		}
		base, err := gitaccess.BaseURL(a.BaseURL)
		if err != nil || base != a.BaseURL {
			return errors.New("OAuth 服务地址必须为规范 HTTPS 地址")
		}
		redirect, err := url.Parse(a.RedirectURL)
		if err != nil || redirect.Host == "" || redirect.User != nil || redirect.RawQuery != "" || redirect.Fragment != "" || redirect.Path != "/api/git/oauth/callback" || redirect.RawPath != "" {
			return errors.New("OAuth 回调地址必须以 /api/git/oauth/callback 结尾")
		}
		if redirect.Scheme != "https" && !(redirect.Scheme == "http" && (redirect.Hostname() == "localhost" || redirect.Hostname() == "127.0.0.1" || redirect.Hostname() == "::1")) {
			return errors.New("OAuth 回调必须使用 HTTPS；仅本机开发允许 HTTP")
		}
		if strings.ContainsAny(a.ClientID+a.Label, "\r\n\x00") {
			return errors.New("OAuth 应用字段不能包含控制字符")
		}
	}
	return nil
}
func (c *Config) SaveGitOAuthApp(app GitOAuthApp, expected int64) error {
	return c.mutate(func(w *Config) error {
		for i, old := range w.GitOAuthApps {
			if old.ID != app.ID {
				continue
			}
			if old.Revision != expected || old.Provider != app.Provider || old.BaseURL != app.BaseURL || old.ClientID != app.ClientID || old.RedirectURL != app.RedirectURL || old.Network != app.Network {
				return ErrGitOAuthConflict
			}
			app.Revision = expected + 1
			app.Secret = slices.Clone(app.Secret)
			w.GitOAuthApps[i] = app
			return nil
		}
		if expected != 0 {
			return ErrGitOAuthConflict
		}
		app.Revision = 1
		app.Secret = slices.Clone(app.Secret)
		w.GitOAuthApps = append(w.GitOAuthApps, app)
		return nil
	})
}

// RekeyGitOAuthApps uses the regular copy/validate/persist path and keeps
// application IDs and revisions stable; cryptographic maintenance changes no
// platform identity or authorization policy.
func (c *Config) RekeyGitOAuthApps(transform func(GitOAuthApp) ([]byte, error)) error {
	return c.mutate(func(w *Config) error {
		for i, app := range w.GitOAuthApps {
			secret, err := transform(app)
			if err != nil {
				return err
			}
			w.GitOAuthApps[i].Secret = secret
		}
		return nil
	})
}
