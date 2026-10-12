package config

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

const DecisionJevModel = "jev-1.13.0"

var ErrDecisionRevision = errors.New("智能决策配置已变化，请刷新后重试")

type DecisionConfig struct {
	Revision        int64           `json:"revision"`
	DefaultProvider string          `json:"default_provider"`
	FallbackPolicy  string          `json:"fallback_policy"`
	Plugins         DecisionPlugins `json:"plugins"`
}
type DecisionPlugins struct {
	Jev DecisionJev `json:"jev"`
}
type DecisionJev struct {
	Enabled         bool           `json:"enabled"`
	Gateway         string         `json:"gateway"`
	Model           string         `json:"model"`
	TimeoutMS       int            `json:"timeout_ms"`
	MaxRequestBytes int            `json:"max_request_bytes"`
	MaxConcurrency  int            `json:"max_concurrency"`
	Access          *AccountAccess `json:"access"`
	APIKey          string         `json:"api_key,omitempty"`
}
type DecisionView struct {
	Revision        int64               `json:"revision"`
	DefaultProvider string              `json:"default_provider"`
	FallbackPolicy  string              `json:"fallback_policy"`
	Plugins         DecisionPluginsView `json:"plugins"`
}
type DecisionPluginsView struct {
	Jev DecisionJevView `json:"jev"`
}
type DecisionJevView struct {
	Enabled         bool           `json:"enabled"`
	Gateway         string         `json:"gateway"`
	Model           string         `json:"model"`
	TimeoutMS       int            `json:"timeout_ms"`
	MaxRequestBytes int            `json:"max_request_bytes"`
	MaxConcurrency  int            `json:"max_concurrency"`
	Access          *AccountAccess `json:"access"`
	KeyConfigured   bool           `json:"key_configured"`
}
type DecisionPatch struct {
	ExpectedRevision *int64                `json:"expected_revision"`
	DefaultProvider  *string               `json:"default_provider"`
	FallbackPolicy   *string               `json:"fallback_policy"`
	Plugins          *DecisionPluginsPatch `json:"plugins"`
}
type DecisionPluginsPatch struct {
	Jev *DecisionJevPatch `json:"jev"`
}
type DecisionJevPatch struct {
	Enabled         *bool                `json:"enabled"`
	Gateway         *string              `json:"gateway"`
	Model           *string              `json:"model"`
	TimeoutMS       *int                 `json:"timeout_ms"`
	MaxRequestBytes *int                 `json:"max_request_bytes"`
	MaxConcurrency  *int                 `json:"max_concurrency"`
	Access          *AccountAccess       `json:"access"`
	APIKey          *DecisionSecretPatch `json:"api_key"`
}
type DecisionSecretPatch struct {
	Action string `json:"action"`
	Value  string `json:"value,omitempty"`
}

func (d DecisionConfig) Clone() DecisionConfig {
	d.Plugins.Jev.Access = cloneAccess(d.Plugins.Jev.Access)
	return d
}
func (d DecisionConfig) normalized() DecisionConfig {
	d = d.Clone()
	if d.Revision == 0 {
		d.Revision = 1
	}
	if d.DefaultProvider == "" {
		d.DefaultProvider = "builtin"
	}
	if d.FallbackPolicy == "" {
		d.FallbackPolicy = "workspace_default"
	}
	j := &d.Plugins.Jev
	if j.Gateway == "" {
		j.Gateway = "typesafe"
	}
	if j.Model == "" {
		j.Model = DecisionJevModel
	}
	if j.TimeoutMS == 0 {
		j.TimeoutMS = 1500
	}
	if j.MaxRequestBytes == 0 {
		j.MaxRequestBytes = 32 << 10
	}
	if j.MaxConcurrency == 0 {
		j.MaxConcurrency = 2
	}
	if j.Access == nil {
		j.Access = &AccountAccess{Mode: "admin"}
	}
	return d
}
func (d DecisionConfig) validate() error {
	if d.Revision < 1 || d.Revision == math.MaxInt64 {
		return fmt.Errorf("decision.revision invalid")
	}
	if d.DefaultProvider != "builtin" && d.DefaultProvider != "jev" {
		return fmt.Errorf("decision.default_provider must be builtin or jev")
	}
	if d.FallbackPolicy != "workspace_default" {
		return fmt.Errorf("decision.fallback_policy must be workspace_default")
	}
	j := d.Plugins.Jev
	if j.Gateway != "typesafe" && j.Gateway != "mindshub" {
		return fmt.Errorf("decision.plugins.jev.gateway must be typesafe or mindshub")
	}
	if j.Model != DecisionJevModel {
		return fmt.Errorf("decision.plugins.jev.model must be %s (verified protocol and pricing)", DecisionJevModel)
	}
	if j.TimeoutMS < 100 || j.TimeoutMS > 30000 {
		return fmt.Errorf("decision.plugins.jev.timeout_ms must be 100..30000")
	}
	if j.MaxRequestBytes < 1024 || j.MaxRequestBytes > 64<<10 {
		return fmt.Errorf("decision.plugins.jev.max_request_bytes must be 1024..65536")
	}
	if j.MaxConcurrency < 1 || j.MaxConcurrency > 16 {
		return fmt.Errorf("decision.plugins.jev.max_concurrency must be 1..16")
	}
	if len(j.APIKey) > 4096 || strings.ContainsAny(j.APIKey, "\r\n\x00") || strings.TrimSpace(j.APIKey) != j.APIKey {
		return fmt.Errorf("decision API key is invalid")
	}
	if err := j.Access.Validate(); err != nil {
		return fmt.Errorf("decision.plugins.jev: %w", err)
	}
	return nil
}
func (j DecisionJev) CanUse(user string, admin bool) bool {
	return Account{Access: j.Access}.CanUse(user, admin)
}
func (d DecisionConfig) View() DecisionView {
	d = d.normalized()
	j := d.Plugins.Jev
	return DecisionView{Revision: d.Revision, DefaultProvider: d.DefaultProvider, FallbackPolicy: d.FallbackPolicy, Plugins: DecisionPluginsView{Jev: DecisionJevView{Enabled: j.Enabled, Gateway: j.Gateway, Model: j.Model, TimeoutMS: j.TimeoutMS, MaxRequestBytes: j.MaxRequestBytes, MaxConcurrency: j.MaxConcurrency, Access: cloneAccess(j.Access), KeyConfigured: j.APIKey != ""}}}
}
func (c *Config) GetDecision() DecisionConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Decision.normalized()
}

// CloneForDecision freezes account/model/default policy under one read lock.
// It is an internal credential-bearing snapshot, never an API response or a
// persistable task value. Only resolved nonsecret candidates leave this copy.
func (c *Config) CloneForDecision() *Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := &Config{Decision: c.Decision.normalized(), Models: cloneModels(c.Models), DefaultModels: c.defaultModels(), Pricing: clonePrices(c.Pricing)}
	out.Accounts = make([]Account, len(c.Accounts))
	for i, a := range c.Accounts {
		out.Accounts[i] = cloneAccount(a)
	}
	return out
}

// DecisionState returns a deep snapshot and an invalidation signal. The signal
// rotates only after a successful persisted decision mutation, never a failed save.
func (c *Config) DecisionState() (DecisionConfig, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.decisionChanged == nil {
		c.decisionChanged = make(chan struct{})
	}
	return c.Decision.normalized(), c.decisionChanged
}
func (c *Config) applyDecisionPatch(p DecisionPatch) error {
	d := c.Decision.normalized()
	if p.ExpectedRevision == nil || *p.ExpectedRevision != d.Revision {
		return ErrDecisionRevision
	}
	if p.DefaultProvider != nil {
		d.DefaultProvider = *p.DefaultProvider
	}
	if p.FallbackPolicy != nil {
		d.FallbackPolicy = *p.FallbackPolicy
	}
	if p.Plugins != nil && p.Plugins.Jev != nil {
		p, j := p.Plugins.Jev, &d.Plugins.Jev
		if p.Gateway != nil {
			// Credentials belong to an endpoint. An ordinary settings change
			// must not deliver a saved key to a different service.
			if *p.Gateway != j.Gateway && j.APIKey != "" && (p.APIKey == nil || (p.APIKey.Action != "set" && p.APIKey.Action != "clear")) {
				return fmt.Errorf("切换 Jev 网关时请设置新 API Key 或明确清除旧密钥")
			}
			j.Gateway = *p.Gateway
		}
		if p.Enabled != nil {
			j.Enabled = *p.Enabled
		}
		if p.Model != nil {
			j.Model = *p.Model
		}
		if p.TimeoutMS != nil {
			j.TimeoutMS = *p.TimeoutMS
		}
		if p.MaxRequestBytes != nil {
			j.MaxRequestBytes = *p.MaxRequestBytes
		}
		if p.MaxConcurrency != nil {
			j.MaxConcurrency = *p.MaxConcurrency
		}
		if p.Access != nil {
			j.Access = cloneAccess(p.Access)
		}
		if p.APIKey != nil {
			switch p.APIKey.Action {
			case "keep":
				if p.APIKey.Value != "" {
					return fmt.Errorf("keep key operation must not carry a value")
				}
			case "clear":
				if p.APIKey.Value != "" {
					return fmt.Errorf("clear key operation must not carry a value")
				}
				j.APIKey = ""
			case "set":
				if p.APIKey.Value == "" {
					return fmt.Errorf("set key operation requires a nonempty value")
				}
				j.APIKey = p.APIKey.Value
			default:
				return fmt.Errorf("key action must be keep, set or clear")
			}
		}
	}
	d.Revision++
	if err := d.validate(); err != nil {
		return err
	}
	c.Decision = d
	return nil
}
