package usage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/store"
)

// Interactive session transcript: two main-conversation requests (the second
// only reads the cache), a subagent sidechain in between, a -p request of
// another process and, last, the cost-state of the CLI leaving the session.
const termCacheFixture = `{"type":"user","entrypoint":"cli","uuid":"u1","sessionId":"conv","cwd":"/workspace/app","timestamp":"2026-10-20T10:00:00.000Z","message":{"content":"hi"}}
{"type":"assistant","entrypoint":"cli","uuid":"a1","sessionId":"conv","cwd":"/workspace/app","requestId":"req_1","effort":"xhigh","timestamp":"2026-10-20T10:00:05.000Z","message":{"id":"msg_1","model":"claude-opus-5-5","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":40,"cache_read_input_tokens":100000,"cache_creation_input_tokens":20000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":20000}}}}
{"type":"assistant","entrypoint":"cli","uuid":"s1","isSidechain":true,"sessionId":"conv","cwd":"/workspace/app","requestId":"req_side","timestamp":"2026-10-20T10:00:06.000Z","message":{"id":"msg_side","model":"claude-haiku-5-5","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":3000,"cache_creation":{"ephemeral_5m_input_tokens":3000,"ephemeral_1h_input_tokens":0}}}}
{"type":"assistant","entrypoint":"sdk-cli","uuid":"p1","sessionId":"conv","cwd":"/workspace/app","requestId":"req_p","timestamp":"2026-10-20T10:00:07.000Z","message":{"id":"msg_p","model":"claude-opus-5-5","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":5,"cache_read_input_tokens":120000,"cache_creation_input_tokens":0}}}
{"type":"user","entrypoint":"cli","uuid":"u2","sessionId":"conv","cwd":"/workspace/app","timestamp":"2026-10-20T10:01:00.000Z","message":{"content":"more"}}
{"type":"assistant","entrypoint":"cli","uuid":"a2","sessionId":"conv","cwd":"/workspace/app","requestId":"req_2","effort":"xhigh","timestamp":"2026-10-20T10:01:04.000Z","message":{"id":"msg_2","model":"claude-opus-5-5","stop_reason":null,"usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":120040,"cache_creation_input_tokens":0}}}
{"type":"assistant","entrypoint":"cli","uuid":"a3","sessionId":"conv","cwd":"/workspace/app","requestId":"req_2","effort":"xhigh","timestamp":"2026-10-20T10:01:09.000Z","message":{"id":"msg_2","model":"claude-opus-5-5","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":30,"cache_read_input_tokens":120040,"cache_creation_input_tokens":0}}}
{"type":"cost-state","timestamp":"2026-10-20T10:05:00.000Z"}
`

func TestClaudeTerminalCachePoint(t *testing.T) {
	lines := strings.SplitAfter(strings.TrimSuffix(termCacheFixture, "\n"), "\n")
	p := newClaudeTermParser()
	for _, line := range lines[:len(lines)-1] {
		p.line([]byte(strings.TrimSuffix(line, "\n")))
	}
	got, ok := p.cachePoint()
	want := TerminalCache{Conversation: "conv", Cwd: "/workspace/app", Model: "claude-opus-5-5", Effort: "xhigh",
		Prompt: PromptCache{Prompt: 120042, Read: 120040, TTL: "1h"}, At: time.Date(2026, 10, 20, 10, 1, 4, 0, time.UTC)}
	if !ok || got != want {
		t.Fatalf("cache point\n got %+v\nwant %+v", got, want)
	}
	// Compacting leaves the cache behind; the next request is a new point.
	p.line([]byte(`{"type":"system","subtype":"compact_boundary","entrypoint":"cli","uuid":"c1","isSidechain":false,"sessionId":"conv","cwd":"/workspace/app","timestamp":"2026-10-20T10:02:00.000Z","compactMetadata":{"trigger":"manual","preTokens":120072,"postTokens":9000}}`))
	compacted := want
	compacted.Prompt = PromptCache{TTL: "1h", Compacted: true}
	if got, ok := p.cachePoint(); !ok || got != compacted {
		t.Fatalf("after compacting\n got %+v\nwant %+v", got, compacted)
	}
	p.line([]byte(`{"type":"assistant","entrypoint":"cli","uuid":"a4","sessionId":"conv","cwd":"/workspace/app","requestId":"req_3","effort":"xhigh","timestamp":"2026-10-20T10:03:00.000Z","message":{"id":"msg_3","model":"claude-opus-5-5","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":30,"cache_read_input_tokens":8000,"cache_creation_input_tokens":1500}}}`))
	if got, _ := p.cachePoint(); got.Prompt != (PromptCache{Prompt: 9502, Read: 8000, TTL: "1h"}) {
		t.Fatalf("request after compacting: %+v", got)
	}
	p.line([]byte(lines[len(lines)-1]))
	if got, _ := p.cachePoint(); !got.Exited {
		t.Fatal("the CLI leaving the session went unnoticed")
	}
	// The scan reports it once, named after the transcript file.
	s, sess := newTestServer(t)
	sess.Agent = "claude"
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace-app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file-id.jsonl"), []byte(strings.Join(lines[:len(lines)-1], "")), 0o600); err != nil {
		t.Fatal(err)
	}
	var heard []TerminalCache
	s.OnTerminalCache(func(got store.Session, c TerminalCache) {
		if got.ID != sess.ID {
			t.Errorf("reported for %q", got.ID)
		}
		heard = append(heard, c)
	})
	sc := newScanner()
	s.scanTerminalUsage(sc)
	s.scanTerminalUsage(sc)
	want.Conversation = "file-id"
	if len(heard) != 1 || heard[0] != want {
		t.Fatalf("reported %+v", heard)
	}
	f, err := os.OpenFile(filepath.Join(dir, "file-id.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(lines[len(lines)-1]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s.scanTerminalUsage(sc)
	if len(heard) != 2 || !heard[1].Exited {
		t.Fatalf("exit not reported: %+v", heard)
	}
}
