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
	if len(rows) != 4 {
		t.Fatalf("want 4 request rows, got %+v", rows)
	}
	// One row per request; repeated token_count and reasoning are not double counted.
	if rows[0].ev.InputTokens != 60 || rows[0].ev.CacheReadTokens != 40 || rows[0].ev.OutputTokens != 10 ||
		rows[1].ev.InputTokens != 30 || rows[1].ev.CacheReadTokens != 20 || rows[1].ev.OutputTokens != 5 {
		t.Fatalf("repeated usage/reasoning double counted %+v / %+v", rows[0], rows[1])
	}
	// The first request keeps the old merged row's key; later ones append a sequence.
	if rows[1].reqID != rows[0].reqID+":1" || rows[1].ev.TurnID != rows[0].ev.TurnID || strings.Contains(rows[0].reqID[len("codex-terminal:"):], ":") {
		t.Fatalf("keys %q / %q", rows[0].reqID, rows[1].reqID)
	}
	if rows[2].ev.InputTokens != 20 || rows[2].ev.OutputTokens != 2 || rows[2].ev.TurnID != rows[0].ev.TurnID {
		t.Fatal(rows[2])
	}
	if rows[3].ev.InputTokens != 10 || rows[3].ev.OutputTokens != 3 || rows[3].ev.TurnID == rows[0].ev.TurnID {
		t.Fatal(rows[3])
	}
	for i, want := range []string{"high", "high", "medium", ""} {
		if rows[i].ev.Effort != want || (want != "" && rows[i].ev.EffortControl != "effort") {
			t.Fatalf("row %d effort = %+v, want %q", i, rows[i].ev, want)
		}
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
	if len(rows) != 4 {
		t.Fatal(rows)
	}
	var fixtureA int64
	for _, r := range rows {
		// The turn's second request first appears after the price edit and
		// still inherits the turn's original snapshot.
		if r.Model == "gpt-fixture-a" && r.Price.Standard.Input != 2 {
			t.Fatalf("growth repriced %+v", r)
		}
		if r.Model == "gpt-fixture-a" {
			fixtureA += r.CostMicroUSD
		}
		if r.Model == "gpt-fixture-a" && (r.Effort != "high" || r.EffortControl != "effort") {
			t.Fatalf("transcript effort not persisted: %+v", r)
		}
	}
	if fixtureA != 225 {
		t.Fatalf("fixture-a cost %d", fixtureA)
	}
	q, _ := s.store.GetQuota(sess.User)
	if q.BalanceMicroUSD != 1000 {
		t.Fatal("terminal debited balance")
	}
}

func TestCodexTerminalEffortStaysUnknownWhenMissingOrMixed(t *testing.T) {
	fixture := codexFixture(t)
	for name, raw := range map[string]string{
		"missing":     strings.ReplaceAll(fixture, `, "effort": "high"`, ""),
		"null":        strings.ReplaceAll(fixture, `"effort": "high"`, `"effort": null`),
		"unsupported": strings.ReplaceAll(fixture, `"effort": "high"`, `"effort": {"future": true}`),
		"mixed":       strings.Replace(fixture, `{"timestamp": "2026-09-23T10:00:05Z"`, `{"type":"turn_context","payload":{"turn_id":"turn-one","model":"gpt-fixture-a","effort":"low"}}`+"\n"+`{"timestamp": "2026-09-23T10:00:05Z"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := parseCodexTerminal(strings.NewReader(raw))
			if err != nil || len(rows) != 4 || rows[0].ev.InputTokens != 60 || rows[1].ev.OutputTokens != 5 {
				t.Fatalf("effort changed usage: %+v, %v", rows, err)
			}
			// Each request carries the effort recorded before it; nothing is invented.
			want := map[string][2]string{"mixed": {"high", "low"}}[name]
			for i, r := range rows[:2] {
				if r.ev.Effort != want[i] || (want[i] == "" && r.ev.EffortControl != "") {
					t.Fatalf("request %d effort = %+v, want %q", i, r.ev, want[i])
				}
			}
		})
	}
}

func TestCodexTerminalEffortContextBeforeStart(t *testing.T) {
	lines := strings.Split(codexFixture(t), "\n")
	lines[1], lines[2] = lines[2], lines[1]
	rows, err := parseCodexTerminal(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil || len(rows) != 4 || rows[0].ev.Effort != "high" {
		t.Fatalf("same-turn start erased recorded context: %+v, %v", rows, err)
	}
}
func TestCodexCounterResetAndPartialTail(t *testing.T) {
	raw := codexFixture(t) + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"reset"}}
{"type":"turn_context","payload":{"turn_id":"reset","model":"gpt-fixture-b"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":0,"output_tokens":0},"last_token_usage":{"input_tokens":180,"output_tokens":20}}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":5,"output_tokens":2},"last_token_usage":{"input_tokens":5,"output_tokens":2}}}}
{"type":"event_msg","payload":`
	rows, err := parseCodexTerminal(strings.NewReader(raw))
	if err != nil || len(rows) != 5 {
		t.Fatalf("partial/reset: %v %+v", err, rows)
	}
	if rows[4].ev.InputTokens != 5 || rows[4].ev.OutputTokens != 2 {
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
	if len(rows) != 8 {
		t.Fatalf("workspace identity lost: %d rows", len(rows))
	}
}
