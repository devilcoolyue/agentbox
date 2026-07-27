package server

import (
	"strings"
	"testing"
)

func TestTmuxEnvSyncMirrorsAndUnsets(t *testing.T) {
	got := tmuxEnvSync([]string{
		"ANTHROPIC_BASE_URL=https://example.com",
		envIntranetProxy + "=socks5h://u:p@172.17.0.1:1080",
	})
	for _, want := range []string{
		"tmux set-environment -g ANTHROPIC_BASE_URL 'https://example.com'; ",
		"tmux set-environment -g " + envIntranetProxy + " 'socks5h://u:p@172.17.0.1:1080'; ",
		// Not injected this time (no port maps) -> must be cleared, not kept.
		"tmux set-environment -gu " + envIntranetMaps + "; ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "-gu "+envIntranetProxy) {
		t.Errorf("proxy was provided, must not be unset:\n%s", got)
	}
}

func TestTmuxEnvSyncUnsetsAllTunnelVarsWhenLinkDown(t *testing.T) {
	got := tmuxEnvSync(nil)
	for _, name := range tunnelEnvNames {
		if !strings.Contains(got, "tmux set-environment -gu "+name+"; ") {
			t.Errorf("%s not unset with no env:\n%s", name, got)
		}
	}
}

func TestTmuxEnvSyncSkipsUnsafeNames(t *testing.T) {
	got := tmuxEnvSync([]string{"BAD;NAME=x", "9LEADING=x", "=novalue", "OK_1=y"})
	for _, bad := range []string{"BAD;NAME", "9LEADING", "novalue"} {
		if strings.Contains(got, bad) {
			t.Errorf("unsafe entry %q leaked into:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "tmux set-environment -g OK_1 'y'; ") {
		t.Errorf("valid name dropped:\n%s", got)
	}
}

func TestShellQuoteNeutralizesInjection(t *testing.T) {
	got := shellQuote(`a'; rm -rf /; echo '`)
	if want := `'a'\''; rm -rf /; echo '\'''`; got != want {
		t.Errorf("shellQuote = %s, want %s", got, want)
	}
}

func TestTermCommandKeepsFallbackAndAttach(t *testing.T) {
	cmd := termCommand([]string{envIntranetProxy + "=socks5h://x"})
	if !strings.HasPrefix(cmd, "command -v tmux >/dev/null || exec /bin/bash\n") {
		t.Errorf("pre-tmux image fallback lost:\n%s", cmd)
	}
	if !strings.HasSuffix(cmd, "exec tmux -u new-session -A -D -s main") {
		t.Errorf("attach must be the last thing exec'd:\n%s", cmd)
	}
	// The sync block must not be able to write to the PTY or abort the attach.
	if !strings.Contains(cmd, "} >/dev/null 2>&1\n") {
		t.Errorf("sync output not silenced:\n%s", cmd)
	}
}
