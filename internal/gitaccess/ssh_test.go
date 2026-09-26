package gitaccess

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestSSHURLsAndProtocolTarget(t *testing.T) {
	base, err := SSHBaseURL("ssh://git@Git.Example.com:22", "git")
	if err != nil || base != "ssh://git.example.com" {
		t.Fatalf("base: %q %v", base, err)
	}
	want := "ssh://git@git.example.com/team/repo.git"
	for _, raw := range []string{"git@git.example.com:team/repo.git", "ssh://git@git.example.com/team/repo.git", "ssh://git.example.com/team/repo.git"} {
		got, err := SSHRepositoryURL(raw, base, "git")
		if err != nil || got != want {
			t.Fatalf("normalize: %q %v", got, err)
		}
	}
	for _, raw := range []string{"ssh://evil.example.com/repo", "ssh://git@git.example.com/a/../repo", "ssh://other@git.example.com/repo", "ssh://git@git.example.com/a';touch%20x", "ssh://git@git.example.com/-option", "ssh://git:password@git.example.com/repo", "ext::git-upload-pack repo"} {
		if _, err := SSHRepositoryURL(raw, base, "git"); err == nil {
			t.Errorf("accepted unsafe SSH URL %q", raw)
		}
	}
	if got, err := SSHBaseURL("ssh://git.example.com:443", "git"); err != nil || got != "ssh://git.example.com:443" {
		t.Fatalf("SSH port 443 lost: %q %v", got, err)
	}
	packet := func(v string) string { return fmt.Sprintf("%04x%s", len(v)+4, v) }
	if v2, err := ReadGitRequest(strings.NewReader(packet("git-upload-pack /ticket\x00host=bridge\x00\x00version=2\x00")), "ticket", false); err != nil || !v2 {
		t.Fatalf("valid request: %v %v", v2, err)
	}
	for _, request := range []string{"git-receive-pack /ticket\x00", "git-upload-pack /other\x00", "git-upload-pack /ticket/../repo\x00"} {
		if _, err := ReadGitRequest(strings.NewReader(packet(request)), "ticket", false); err == nil {
			t.Errorf("accepted request %q", request)
		}
	}
	var output bytes.Buffer
	old, next := strings.Repeat("0", 40), strings.Repeat("a", 40)
	command := old + " " + next + " refs/heads/main\x00report-status\n"
	data := packet(command) + "0000PACKfixture"
	if _, err = ForwardPush(&output, strings.NewReader(data), old, next, "refs/heads/main"); err != nil || output.String() != data {
		t.Fatalf("push wire changed: %v", err)
	}
	output.Reset()
	if _, err = ForwardPush(&output, strings.NewReader(data), old, next, "refs/heads/other"); err == nil || output.Len() != 0 {
		t.Fatal("wrong branch bytes reached upstream")
	}
}
