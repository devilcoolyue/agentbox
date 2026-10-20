package server

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
	"agentbox/internal/usage"
)

// The fixtures copy the shape of real Codex 0.163.0 exports — a ChatGPT login
// over the Responses WebSocket (warm-up request, tool round trip, an
// interrupted request) and an API key over HTTP (a record per stream event, a
// failed attempt) — with synthetic values, ids and identity attributes.
func readCodexFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/otlp-logs-codex-0.163.0-" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func codexFixtureEvents(t *testing.T, name string) []codexTelemetryEvent {
	t.Helper()
	records, err := decodeTelemetry(readCodexFixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		for k := range r {
			if !telemetryKeys[k] {
				t.Fatalf("attribute %q kept", k)
			}
		}
	}
	return codexTelemetryEvents(records)
}

const (
	codexWSConversation   = "01a1581b-0000-7000-8000-00000000c0de"
	codexHTTPConversation = "01a15802-0000-7000-8000-0000000000aa"
)

func codexTiming(conv string, in, cached, out, ttft, dur int64) store.RequestTiming {
	return store.RequestTiming{RequestID: usage.CodexRequestKey(conv, in, cached, out), TTFTMs: ttft, DurationMS: dur}
}

func TestCodexTelemetryPairs(t *testing.T) {
	evs := codexFixtureEvents(t, "websocket")
	want := []store.RequestTiming{
		// Duration: request record (less its send time) to the completion
		// record. TTFT: ttft_ms plus the send time it does not include.
		codexTiming(codexWSConversation, 14353, 12288, 167, 2845, 8558),
		codexTiming(codexWSConversation, 14543, 12288, 96, 2562, 4790),
		codexTiming(codexWSConversation, 14820, 14208, 43, 2690, 3477),
		// The interrupted request is replaced by the next one.
		codexTiming(codexWSConversation, 15001, 12288, 106, 3137, 8866),
	}
	var p codexPairing
	now := time.Now()
	got, background := p.feed("s1", evs, now)
	if !slices.Equal(got, want) {
		t.Fatalf("timings = %+v", got)
	}
	// Every completion is a background candidate; the warm-up (no output, no
	// ttft_ms) is marked as one that always gets a row.
	wantBackground := []codexBackground{
		{Conversation: codexWSConversation, AtMS: background[0].AtMS, DurationMS: 591, Input: 12498, Model: "gpt-6.1-sol", Originator: "agentbox", Warmup: true},
		{Conversation: codexWSConversation, AtMS: background[1].AtMS, DurationMS: 8558, TTFTMs: 2845, Input: 14353, Cached: 12288, Output: 167, Model: "gpt-6.1-sol", Effort: "medium", Originator: "agentbox"},
	}
	if len(background) != 5 || !slices.Equal(background[:2], wantBackground) || background[4].Effort != "high" {
		t.Fatalf("background = %+v", background)
	}
	// Request and completion usually land in different export batches.
	for split := 1; split < len(evs); split++ {
		var p codexPairing
		first, firstBG := p.feed("s1", evs[:split], now)
		second, secondBG := p.feed("s1", evs[split:], now)
		if got := append(first, second...); !slices.Equal(got, want) || !slices.Equal(append(firstBG, secondBG...), background) {
			t.Fatalf("split at %d: %+v", split, got)
		}
	}
	// Another session's completion does not close this session's request.
	var q codexPairing
	q.feed("s1", evs[:7], now) // through the first real request
	if got, _ := q.feed("s2", evs[7:9], now); len(got) != 0 {
		t.Fatalf("paired across sessions: %+v", got)
	}

	// Over HTTP the request record is written when the headers arrive.
	var h codexPairing
	got, _ = h.feed("s1", codexFixtureEvents(t, "http"), now)
	if want := []store.RequestTiming{codexTiming(codexHTTPConversation, 1200, 1000, 40, 1236, 1841)}; !slices.Equal(got, want) {
		t.Fatalf("http timings = %+v", got)
	}
}

func writeRollout(t *testing.T, s *Server, sess store.Session, conversation string) {
	t.Helper()
	dir := filepath.Join(s.homeDir(sess), ".codex", "sessions", "2026", "10", "20")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-20T06-59-08-"+conversation+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Requests no other source accounts for become background rows: the warm-up
// of every start, and conversations that keep no rollout (the terminal UI's
// side conversation). They never move the balance.
func TestUsageTelemetryCodexBackgroundRows(t *testing.T) {
	s, sess := telemetryServer(t)
	sess.Agent, sess.AccountID = config.AgentCodex, "sub"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	s.cfg.Pricing = map[string]config.ModelPrice{"gpt-6.1-sol": {TokenRates: config.TokenRates{Input: 1, CacheRead: 0.1, Output: 10}}}
	if _, err := s.store.Grant(sess.User, 1_000_000, "grant:test", "", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.SetQuotaEnforced(sess.User, true); err != nil {
		t.Fatal(err)
	}
	writeRollout(t, s, sess, codexWSConversation)
	path := "/otel/" + sess.ID + "/" + s.usageTelemetryMAC(sess.ID) + "/v1/logs"
	post := func(body []byte) {
		t.Helper()
		if w := postTelemetry(s, path, body, nil); w.Code != http.StatusOK {
			t.Fatalf("export = %d %s", w.Code, w.Body)
		}
	}
	background := func() []store.UsageEvent {
		var out []store.UsageEvent
		for _, r := range s.store.ListUsage(store.UsageFilter{User: sess.User}) {
			if r.Kind == store.UsageKindBackground {
				out = append(out, r)
			}
		}
		return out
	}
	post(readCodexFixture(t, "websocket"))
	rows := background()
	if len(rows) != 1 {
		t.Fatalf("background rows = %+v", rows)
	}
	warm := rows[0]
	if warm.InputTokens != 12498 || warm.OutputTokens != 0 || warm.Model != "gpt-6.1-sol" || warm.SessionID != sess.ID ||
		warm.APIDurationMS != 591 || warm.APITTFTMs != 0 || warm.CostMicroUSD != 12498 || warm.Price == nil || !strings.Contains(warm.Raw, `"warmup":true`) {
		t.Fatalf("warm-up row %+v", warm)
	}
	// A resent export adds nothing.
	post(readCodexFixture(t, "websocket"))
	if n := len(background()); n != 1 {
		t.Fatalf("resent export added rows: %d", n)
	}
	// A web conversation without a rollout (one is always written; here the
	// test left it out) still gets only its warm-up: web settlement records
	// the rest when the turn ends.
	web := strings.ReplaceAll(string(readCodexFixture(t, "websocket")), codexWSConversation, "01a1584a-0000-7000-8000-00000000we80")
	post([]byte(web))
	if n := len(background()); n != 2 {
		t.Fatalf("web conversation rows = %d", n)
	}
	// The terminal UI's side conversation keeps no rollout: its requests are
	// counted from telemetry.
	side := strings.ReplaceAll(string(readCodexFixture(t, "websocket")), codexWSConversation, "01a1584a-0000-7000-8000-0000000051de")
	side = strings.ReplaceAll(side, `"stringValue": "agentbox"`, `"stringValue": "codex-tui"`)
	post([]byte(side))
	if n := len(background()); n != 2+5 {
		t.Fatalf("side conversation rows = %d", n)
	}
	// A terminal-UI conversation with a rollout is the terminal backfill's.
	tui := strings.ReplaceAll(string(readCodexFixture(t, "websocket")), `"stringValue": "agentbox"`, `"stringValue": "codex-tui"`)
	tui = strings.ReplaceAll(tui, "2026-10-20T0", "2026-10-21T0") // new completion times
	post([]byte(tui))
	if n := len(background()); n != 2+5+1 {
		t.Fatalf("terminal conversation with a rollout rows = %d", n)
	}
	for _, r := range background() {
		if strings.Contains(r.Raw, "51de") && r.OutputTokens == 167 && (r.APITTFTMs != 2845 || r.APIDurationMS != 8558 || r.Effort != "medium") {
			t.Fatalf("side request row %+v", r)
		}
	}
	if q, _ := s.store.GetQuota(sess.User); q.BalanceMicroUSD != 1_000_000 {
		t.Fatalf("background rows moved the balance: %+v", q)
	}

	// A hidden delegation worker's rows go to the workspace it serves.
	worker, err := s.store.CreateWorkerSession(sess.ID, store.Session{ID: "w1", User: sess.User, Name: "worker", Agent: config.AgentCodex, AccountID: "sub"})
	if err != nil {
		t.Fatal(err)
	}
	wpath := "/otel/" + worker.ID + "/" + s.usageTelemetryMAC(worker.ID) + "/v1/logs"
	workerExport := strings.ReplaceAll(string(readCodexFixture(t, "websocket")), codexWSConversation, "01a1584a-0000-7000-8000-00000000w0rk")
	if w := postTelemetry(s, wpath, []byte(workerExport), nil); w.Code != http.StatusOK {
		t.Fatalf("worker export = %d", w.Code)
	}
	var workerRows int
	for _, r := range background() {
		if strings.Contains(r.Raw, "w0rk") {
			workerRows++
			if r.SessionID != sess.ID || r.AccountID != "sub" {
				t.Fatalf("worker row %+v", r)
			}
		}
	}
	if workerRows != 1 {
		t.Fatalf("worker warm-up rows = %d", workerRows)
	}
}

func TestCodexTelemetryPendingBounded(t *testing.T) {
	var p codexPairing
	old := time.Now().Add(-2 * codexPendingKeep)
	for i := 0; i < codexPendingMax; i++ {
		p.feed("s", []codexTelemetryEvent{{Conversation: string(rune('a'+i%26)) + time.Duration(i).String(), Request: true, AtMS: 1}}, old)
	}
	p.feed("s", []codexTelemetryEvent{{Conversation: "fresh", Request: true, AtMS: 1}}, time.Now())
	if n := len(p.pending); n > codexPendingMax/2+1 {
		t.Fatalf("pending = %d", n)
	}
}

func TestUsageTelemetryCodexFillsRows(t *testing.T) {
	s, sess := telemetryServer(t)
	sess.Agent = config.AgentCodex
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	writeRollout(t, s, sess, codexWSConversation)
	web := codexTiming(codexWSConversation, 14353, 12288, 167, 0, 0).RequestID
	if err := s.store.InsertUsage(store.UsageEvent{User: sess.User, SessionID: sess.ID, TurnID: "t", Agent: "codex",
		InputTokens: 14353 - 12288, CacheReadTokens: 12288, OutputTokens: 167, APIRequestID: web, TS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	path := "/otel/" + sess.ID + "/" + s.usageTelemetryMAC(sess.ID) + "/v1/logs"
	if w := postTelemetry(s, path, readCodexFixture(t, "websocket"), nil); w.Code != http.StatusOK {
		t.Fatalf("export = %d %s", w.Code, w.Body)
	}
	rows := s.store.ListUsage(store.UsageFilter{User: sess.User, Kind: store.UsageKindChat})
	if len(rows) != 1 || rows[0].APITTFTMs != 2845 || rows[0].APIDurationMS != 8558 {
		t.Fatalf("rows = %+v", rows)
	}
	// A terminal row scanned after the export claims its waiting timing.
	term := codexTiming(codexWSConversation, 14820, 14208, 43, 0, 0).RequestID
	if err := s.store.UpsertTerminalUsage(store.UsageEvent{User: sess.User, SessionID: sess.ID, TurnID: "t2", Agent: "codex",
		ReqID: "codex-terminal:x", APIRequestID: term, TS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.store.ListUsage(store.UsageFilter{User: sess.User}) {
		if r.APIRequestID == term && (r.APITTFTMs != 2690 || r.APIDurationMS != 3477) {
			t.Fatalf("waiting timing not claimed: %+v", r)
		}
	}
	// A Claude session's export never feeds the Codex pairing.
	claude, _ := telemetryServer(t)
	cpath := "/otel/" + sess.ID + "/" + claude.usageTelemetryMAC(sess.ID) + "/v1/logs"
	if w := postTelemetry(claude, cpath, readCodexFixture(t, "websocket"), nil); w.Code != http.StatusOK || len(claude.codexPairs.pending) != 0 {
		t.Fatalf("claude session export = %d, pending %d", w.Code, len(claude.codexPairs.pending))
	}
}

func TestCodexTelemetryEndpoint(t *testing.T) {
	s, sess := telemetryServer(t)
	codex := sess
	codex.Agent = config.AgentCodex
	if got := s.codexTelemetryEndpoint(codex); got != "" {
		t.Fatalf("receiver down but endpoint %q", got)
	}
	s.usageTel.up.Store(true)
	if got := s.codexTelemetryEndpoint(codex); got != "http://172.17.0.1:1084/otel/s1/"+s.usageTelemetryMAC("s1")+"/v1/logs" {
		t.Fatalf("endpoint = %q", got)
	}
	if got := s.codexTelemetryEndpoint(sess); got != "" {
		t.Fatalf("Claude session got a Codex endpoint: %q", got)
	}
	// Codex reads the URL from config.toml: its env only keeps the receiver
	// out of a bound account proxy.
	env := s.usageTelemetryEnv(codex, []string{"HTTPS_PROXY=http://u:p@172.17.0.1:1081", "NO_PROXY=localhost"})
	if v, _ := telemetryEnvValue(env, "NO_PROXY"); v != "localhost,172.17.0.1" {
		t.Fatalf("NO_PROXY = %q", v)
	}
	if _, ok := telemetryEnvValue(env, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"); ok {
		t.Fatalf("Codex got the Claude env: %v", env)
	}
}
