package usage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func codexFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex-terminal-0.145.0.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func TestCodexTerminalDeduplicatesAndGroupsModels(t *testing.T) {
	rows, err := parseCodexTerminal(strings.NewReader(codexFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 turn/model rows, got %+v", rows)
	}
	if rows[0].ev.InputTokens != 90 || rows[0].ev.CacheReadTokens != 60 || rows[0].ev.OutputTokens != 15 {
		t.Fatalf("repeated usage/reasoning double counted %+v", rows[0])
	}
	if rows[1].ev.InputTokens != 20 || rows[1].ev.OutputTokens != 2 || rows[1].ev.TurnID != rows[0].ev.TurnID {
		t.Fatal(rows[1])
	}
	if rows[2].ev.InputTokens != 10 || rows[2].ev.OutputTokens != 3 || rows[2].ev.TurnID == rows[0].ev.TurnID {
		t.Fatal(rows[2])
	}
	for _, origin := range []string{"codex_exec", "codex-app-server", "unknown"} {
		raw := strings.ReplaceAll(codexFixture(t), "codex-tui", origin)
		rows, err := parseCodexTerminal(strings.NewReader(raw))
		if err != nil || len(rows) != 0 {
			t.Fatalf("nonterminal usage accepted: %s", origin)
		}
	}
}
func TestCodexTerminalScanUpdatesWithoutChargingAndKeepsSnapshot(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "codex"
	sess.AccountID = "acct"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	s.cfg.Pricing = map[string]config.ModelPrice{"codex": {TokenRates: config.TokenRates{Input: 2, Output: 3}}}
	if _, err := s.store.Grant(sess.User, 1000, "test-grant", "", "admin"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.homeDir(sess), ".codex", "sessions", "2026", "09", "23", "rollout-fixture.jsonl")
	os.MkdirAll(filepath.Dir(path), 0755)
	fixture := codexFixture(t)
	lines := strings.Split(fixture, "\n")
	if err := os.WriteFile(path, []byte(strings.Join(lines[:5], "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Scan()
	rows := s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal})
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	s.cfg.Pricing["codex"] = config.ModelPrice{TokenRates: config.TokenRates{Input: 99, Output: 99}}
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	s.Scan()
	s.Scan()
	rows = s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal})
	if len(rows) != 3 {
		t.Fatal(rows)
	}
	for _, r := range rows {
		if r.Model == "gpt-fixture-a" && (r.Price.Standard.Input != 2 || r.CostMicroUSD != 225) {
			t.Fatalf("growth repriced %+v", r)
		}
	}
	q, _ := s.store.GetQuota(sess.User)
	if q.BalanceMicroUSD != 1000 {
		t.Fatal("terminal debited balance")
	}
}
func TestCodexCounterResetAndPartialTail(t *testing.T) {
	raw := codexFixture(t) + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"reset"}}
{"type":"turn_context","payload":{"turn_id":"reset","model":"gpt-fixture-b"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":0,"output_tokens":0},"last_token_usage":{"input_tokens":180,"output_tokens":20}}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":5,"output_tokens":2},"last_token_usage":{"input_tokens":5,"output_tokens":2}}}}
{"type":"event_msg","payload":`
	rows, err := parseCodexTerminal(strings.NewReader(raw))
	if err != nil || len(rows) != 4 {
		t.Fatalf("partial/reset: %v %+v", err, rows)
	}
	if rows[3].ev.InputTokens != 5 || rows[3].ev.OutputTokens != 2 {
		t.Fatal("reset duplicated previous usage")
	}
}

func TestCodexCopiesInDifferentWorkspacesDoNotOverwrite(t *testing.T) {
	s, sess := newTestServer(t)
	sess.Agent = "codex"
	fixture := codexFixture(t)
	for _, id := range []string{"one", "two"} {
		sess.ID = id
		if err := s.store.Put(sess); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(s.homeDir(sess), ".codex", "sessions", "2026", "09", "23", "rollout-fixture.jsonl")
		os.MkdirAll(filepath.Dir(path), 0755)
		if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s.Scan()
	rows := s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal})
	if len(rows) != 6 {
		t.Fatalf("workspace identity lost: %d rows", len(rows))
	}
}
