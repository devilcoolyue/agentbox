package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func decisionTestConfig(t *testing.T) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"auth_token":"fixture-token-long-enough","data_dir":"data","accounts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestDecisionDefaultsPersistenceSecretsAndCAS(t *testing.T) {
	c := decisionTestConfig(t)
	d, changed := c.DecisionState()
	if d.Revision != 1 || d.DefaultProvider != "builtin" || d.Plugins.Jev.Enabled || d.Plugins.Jev.Model != DecisionJevModel || d.Plugins.Jev.Access.Mode != "admin" {
		t.Fatal(d)
	}
	enabled := true
	access := &AccountAccess{Mode: "users", Users: []string{"alice"}}
	patch := DecisionPatch{ExpectedRevision: &d.Revision, Plugins: &DecisionPluginsPatch{Jev: &DecisionJevPatch{Enabled: &enabled, Access: access, APIKey: &DecisionSecretPatch{Action: "set", Value: "private-test-key"}}}}
	if err := c.ApplySettings(SettingsPatch{Decision: &patch}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("successful update did not invalidate generation")
	}
	if err := c.ApplySettings(SettingsPatch{Decision: &patch}); !errors.Is(err, ErrDecisionRevision) {
		t.Fatal(err)
	}
	access.Users[0] = "mutated"
	d = c.GetDecision()
	if d.Plugins.Jev.Access.Users[0] != "alice" || d.Revision != 2 {
		t.Fatal(d)
	}
	view, _ := json.Marshal(d.View())
	if strings.Contains(string(view), "private-test-key") || strings.Contains(string(view), `"api_key"`) || !strings.Contains(string(view), `"key_configured":true`) {
		t.Fatal(string(view))
	}
	maxUpload := int64(17)
	if err := c.ApplySettings(SettingsPatch{MaxUploadMB: &maxUpload}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.Path())
	if err != nil || reloaded.GetDecision().Plugins.Jev.APIKey != "private-test-key" || reloaded.GetDecision().Revision != 2 {
		t.Fatal(reloaded, err)
	}
	info, err := os.Stat(c.Path())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	d = c.GetDecision()
	if err := c.ApplySettings(SettingsPatch{Decision: &DecisionPatch{ExpectedRevision: &d.Revision, Plugins: &DecisionPluginsPatch{Jev: &DecisionJevPatch{APIKey: &DecisionSecretPatch{Action: "clear"}}}}}); err != nil {
		t.Fatal(err)
	}
	if c.GetDecision().View().Plugins.Jev.KeyConfigured {
		t.Fatal("clear failed")
	}
}
func TestDecisionFailedUpdatesDoNotInvalidateOrLeak(t *testing.T) {
	c := decisionTestConfig(t)
	d, changed := c.DecisionState()
	invalid := 0
	patch := DecisionPatch{ExpectedRevision: &d.Revision, Plugins: &DecisionPluginsPatch{Jev: &DecisionJevPatch{TimeoutMS: &invalid, APIKey: &DecisionSecretPatch{Action: "set", Value: "should-not-persist"}}}}
	if err := c.ApplySettings(SettingsPatch{Decision: &patch}); err == nil {
		t.Fatal("invalid explicit zero accepted")
	}
	select {
	case <-changed:
		t.Fatal("failed validation invalidated config")
	default:
	}
	if c.GetDecision().Revision != d.Revision || c.GetDecision().Plugins.Jev.APIKey != "" {
		t.Fatal("failed update changed state")
	}
	badKey := DecisionSecretPatch{Action: "keep", Value: "leaked-value"}
	patch.Plugins.Jev.TimeoutMS = nil
	patch.Plugins.Jev.APIKey = &badKey
	if err := c.ApplySettings(SettingsPatch{Decision: &patch}); err == nil || strings.Contains(err.Error(), badKey.Value) {
		t.Fatal(err)
	}
	patch.Plugins.Jev.APIKey = &DecisionSecretPatch{Action: "set", Value: "valid-secret"}
	original := c.path
	c.path = filepath.Join(t.TempDir(), "missing", "config.json")
	if err := c.ApplySettings(SettingsPatch{Decision: &patch}); err == nil {
		t.Fatal("expected save failure")
	}
	c.path = original
	select {
	case <-changed:
		t.Fatal("failed save invalidated config")
	default:
	}
	if c.GetDecision().Plugins.Jev.APIKey != "" {
		t.Fatal("save failure exposed staged secret")
	}
}
func TestDecisionRoutingSnapshotIsIndependent(t *testing.T) {
	c := decisionTestConfig(t)
	if err := c.ApplySettings(SettingsPatch{Pricing: map[string]ModelPrice{"first-model": {TokenRates: TokenRates{Input: 1, Output: 2}, LongContextOver: 1000, Long: &TokenRates{Input: 3, Output: 4}}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddAccount(Account{ID: "first", Type: AgentCodex, Label: "first", Access: &AccountAccess{Mode: "users", Users: []string{"alice"}}, Models: []ModelOption{{ID: "first-model", Label: "First"}}}); err != nil {
		t.Fatal(err)
	}
	snapshot := c.CloneForDecision()
	snapshot.Accounts[0].Access.Users[0] = "outsider"
	snapshot.Models[AgentCodex][0].Label = "changed"
	snapshot.Decision.Plugins.Jev.Access.Mode = "all"
	price := snapshot.Pricing["first-model"]
	price.Long.Input = 999
	price.Input = 998
	snapshot.Pricing["first-model"] = price
	if live := c.GetPricing()["first-model"]; live.Input != 1 || live.Long.Input != 3 {
		t.Fatal("pricing snapshot aliases live config", live)
	}
	acct, _ := c.Account("first")
	if acct.Access.Users[0] != "alice" || c.GetModels()[AgentCodex][0].Label == "changed" || c.GetDecision().Plugins.Jev.Access.Mode != "admin" {
		t.Fatal("snapshot aliases live config")
	}
	d := c.GetDecision()
	d.Plugins.Jev.Access.Mode = "all"
	if c.GetDecision().Plugins.Jev.CanUse("alice", false) {
		t.Fatal("default admin access escaped")
	}
}

func TestDecisionConcurrentCASAndReadSnapshots(t *testing.T) {
	c := decisionTestConfig(t)
	d, changed := c.DecisionState()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.ApplySettings(SettingsPatch{Decision: &DecisionPatch{ExpectedRevision: &d.Revision}})
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := c.Check(); err != nil {
					t.Error(err)
				}
				_ = c.CloneForDecision()
				_, _ = c.DecisionState()
			}
		}()
	}
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrDecisionRevision) {
			t.Fatal(err)
		}
	}
	if winners != 1 || c.GetDecision().Revision != 2 {
		t.Fatal("CAS failed", winners, c.GetDecision().Revision)
	}
	select {
	case <-changed:
	default:
		t.Fatal("successful revision not signaled")
	}
}

func TestDecisionGatewayChangeRequiresNewCredentialIntent(t *testing.T) {
	c := decisionTestConfig(t)
	if c.GetDecision().Plugins.Jev.Gateway != "typesafe" {
		t.Fatal("legacy gateway default changed")
	}
	set := func(p DecisionJevPatch) error {
		rev := c.GetDecision().Revision
		return c.ApplySettings(SettingsPatch{Decision: &DecisionPatch{ExpectedRevision: &rev, Plugins: &DecisionPluginsPatch{Jev: &p}}})
	}
	if err := set(DecisionJevPatch{APIKey: &DecisionSecretPatch{Action: "set", Value: "typesafe-test-key"}}); err != nil {
		t.Fatal(err)
	}
	before, changed := c.DecisionState()
	gateway := "mindshub"
	for _, key := range []*DecisionSecretPatch{nil, {Action: "keep"}} {
		if err := set(DecisionJevPatch{Gateway: &gateway, APIKey: key}); err == nil {
			t.Fatal("saved key would cross endpoint")
		}
	}
	select {
	case <-changed:
		t.Fatal("failed gateway change invalidated config")
	default:
	}
	if got := c.GetDecision(); got.Revision != before.Revision || got.Plugins.Jev.Gateway != "typesafe" || got.Plugins.Jev.APIKey != "typesafe-test-key" {
		t.Fatal("failed switch changed config")
	}
	if err := set(DecisionJevPatch{Gateway: &gateway, APIKey: &DecisionSecretPatch{Action: "set", Value: "mindshub-test-key"}}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.GetDecision(); got.Plugins.Jev.Gateway != "mindshub" || got.Plugins.Jev.APIKey != "mindshub-test-key" || got.View().Plugins.Jev.Gateway != "mindshub" {
		t.Fatal(got.View())
	}
	gateway = "typesafe"
	if err := set(DecisionJevPatch{Gateway: &gateway, APIKey: &DecisionSecretPatch{Action: "clear"}}); err != nil {
		t.Fatal(err)
	}
	if c.GetDecision().Plugins.Jev.APIKey != "" {
		t.Fatal("gateway clear kept key")
	}
	gateway = "mindshub"
	if err := set(DecisionJevPatch{Gateway: &gateway}); err != nil {
		t.Fatal("keyless switch should work", err)
	}
	gateway = "https://untrusted.invalid/"
	if err := set(DecisionJevPatch{Gateway: &gateway}); err == nil {
		t.Fatal("arbitrary gateway accepted")
	}
}
