package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCodexProviderTOMLFresh(t *testing.T) {
	dir := t.TempDir()
	if err := writeCodexProviderTOML(dir, "https://api.example.com", "responses"); err != nil {
		t.Fatal(err)
	}
	base, wire := readCodexProvider(dir)
	if base != "https://api.example.com" || wire != "responses" {
		t.Fatalf("round-trip = %q, %q", base, wire)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	for _, want := range []string{`model_provider = "agentbox"`, "requires_openai_auth = true"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("template missing %q:\n%s", want, raw)
		}
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
	base, wire := readCodexProvider(dir)
	if base != "https://new.example.com" || wire != "chat" {
		t.Fatalf("round-trip = %q, %q", base, wire)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	// 手工调优的其余键必须原样保留，provider 名不被改写
	for _, want := range []string{`model = "gpt-5.5"`, `model_provider = "OpenAI"`, `trust_level = "trusted"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("in-place edit lost %q:\n%s", want, raw)
		}
	}
}

func TestWriteCodexProviderTOMLAppend(t *testing.T) {
	dir := t.TempDir()
	orig := "model = \"gpt-5.5\"\nmodel_reasoning_effort = \"high\"\n"
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(orig), 0o600)
	if err := writeCodexProviderTOML(dir, "https://api.example.com", "responses"); err != nil {
		t.Fatal(err)
	}
	base, wire := readCodexProvider(dir)
	if base != "https://api.example.com" || wire != "responses" {
		t.Fatalf("round-trip = %q, %q", base, wire)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if !strings.Contains(string(raw), `model_provider = "agentbox"`) {
		t.Errorf("append path missing model_provider:\n%s", raw)
	}
	if !strings.Contains(string(raw), "model_reasoning_effort = \"high\"") {
		t.Errorf("append path lost existing keys:\n%s", raw)
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
	base, wire := readCodexProvider(dir)
	if base != "https://new.example.com" || wire != "chat" {
		t.Fatalf("round-trip = %q, %q", base, wire)
	}
}
