package credentials

import (
	"github.com/pelletier/go-toml/v2"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCodexProviderTOMLFresh(t *testing.T) {
	dir := t.TempDir()
	if err := writeCodexProviderTOML(dir, "https://api.example.com", "responses"); err != nil {
		t.Fatal(err)
	}
	base, wire := ReadCodexProvider(dir)
	if base != "https://api.example.com" || wire != "responses" {
		t.Fatalf("round-trip = %q, %q", base, wire)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	var got struct {
		Provider  string `toml:"model_provider"`
		Providers map[string]struct {
			Auth bool `toml:"requires_openai_auth"`
		} `toml:"model_providers"`
	}
	if err := toml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider != "agentbox" || !got.Providers["agentbox"].Auth {
		t.Fatalf("bad provider: %s", raw)
	}

}

func TestWriteCodexProviderTOMLInPlace(t *testing.T) {
	dir := t.TempDir()
	orig := `model_provider = "OpenAI"
model = "gpt-5.5"

[model_providers.OpenAI]
name = "OpenAI"
base_url = "https://old.example.com"
wire_api = "responses"
requires_openai_auth = true

[projects."/root"]
trust_level = "trusted"
`
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(orig), 0o600)
	if err := writeCodexProviderTOML(dir, "https://new.example.com", "chat"); err != nil {
		t.Fatal(err)
	}
	base, wire := ReadCodexProvider(dir)
	if base != "https://new.example.com" || wire != "chat" {
		t.Fatalf("round-trip = %q, %q", base, wire)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	var got struct {
		Model    string
		Provider string `toml:"model_provider"`
		Projects map[string]struct {
			Trust string `toml:"trust_level"`
		}
	}
	if err := toml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "gpt-5.5" || got.Provider != "agentbox" || got.Projects["/root"].Trust != "trusted" {
		t.Fatalf("unrelated settings lost: %s", raw)
	}

}

func TestWriteCodexProviderTOMLAppend(t *testing.T) {
	dir := t.TempDir()
	orig := "model = \"gpt-5.5\"\nmodel_reasoning_effort = \"high\"\n"
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(orig), 0o600)
	if err := writeCodexProviderTOML(dir, "https://api.example.com", "responses"); err != nil {
		t.Fatal(err)
	}
	base, wire := ReadCodexProvider(dir)
	if base != "https://api.example.com" || wire != "responses" {
		t.Fatalf("round-trip = %q, %q", base, wire)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	var got struct {
		Provider  string `toml:"model_provider"`
		Reasoning string `toml:"model_reasoning_effort"`
	}
	if err := toml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider != "agentbox" || got.Reasoning != "high" {
		t.Fatalf("bad config: %s", raw)
	}

}

func TestWriteCodexProviderTOMLAddsMissingWireAPI(t *testing.T) {
	dir := t.TempDir()
	orig := `model_provider = "X"

[model_providers.X]
base_url = "https://old.example.com"
requires_openai_auth = true
`
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(orig), 0o600)
	if err := writeCodexProviderTOML(dir, "https://new.example.com", "chat"); err != nil {
		t.Fatal(err)
	}
	base, wire := ReadCodexProvider(dir)
	if base != "https://new.example.com" || wire != "chat" {
		t.Fatalf("round-trip = %q, %q", base, wire)
	}
}
