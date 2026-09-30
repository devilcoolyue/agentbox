package dockerx

import (
	"strings"
	"testing"

	"agentbox/internal/imageupdate"
)

func TestCLIImagePreservesBaseAndValidatesExecutables(t *testing.T) {
	base := imageupdate.Image{ID: "sha256:" + strings.Repeat("a", 64), User: "1000:1000", Browser: true}
	text, err := cliDockerfile(base, imageupdate.Versions{Claude: "2.1.285", Codex: "0.156.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"FROM " + base.ID, "USER 1000:1000", "claude --version", "codex --version", "/opt/agentbox/browser.py", "agentbox.claude-code"} {
		if !strings.Contains(text, part) {
			t.Fatalf("missing %s", part)
		}
	}
	if _, err := cliDockerfile(base, imageupdate.Versions{Claude: "latest", Codex: "0.156.1"}); err == nil {
		t.Fatal("unpinned version accepted")
	}
	base.User = "root\nRUN evil"
	if _, err := cliDockerfile(base, imageupdate.Versions{Claude: "2.1.285", Codex: "0.156.1"}); err == nil {
		t.Fatal("Dockerfile injection accepted")
	}
}
func TestBuildStreamErrors(t *testing.T) {
	for _, raw := range []string{`{"stream":"step\n"}` + "\n" + `{"errorDetail":{"message":"npm failed"}}`, `{"error":"build failed"}`, `{"stream":`} {
		if err := readBuildOutput(strings.NewReader(raw), func(string) {}); err == nil {
			t.Fatalf("ignored build failure: %s", raw)
		}
	}
	var log string
	if err := readBuildOutput(strings.NewReader(`{"stream":"ok"}`), func(s string) { log += s }); err != nil || log != "ok" {
		t.Fatalf("%s %v", log, err)
	}
}
