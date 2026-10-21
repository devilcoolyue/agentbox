package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Shapes as Claude Code 2.1.280 / 2.1.304 write them (stream-json user events).
const (
	readResult = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_read","type":"tool_result","content":"1\tline one\n2\tline two"}]},"tool_use_result":{"type":"text","file":{"filePath":"/workspace/notes.txt","content":"SECRET FILE BODY","numLines":2,"startLine":1,"totalLines":2}}}`
	editResult = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_edit","type":"tool_result","content":"The file /workspace/a.py has been updated successfully."}]},"tool_use_result":{"filePath":"/workspace/a.py","oldString":"print(1)","newString":"print(2)","originalFile":"WHOLE ORIGINAL FILE","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-print(1)","+print(2)"]}],"userModified":false,"replaceAll":false}}`
	failResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"Exit code 3\n2","is_error":true,"tool_use_id":"toolu_fail"}]},"tool_use_result":"Error: Exit code 3\n2"}`
	subResult  = `{"type":"user","parent_tool_use_id":"toolu_agent","message":{"role":"user","content":[{"tool_use_id":"toolu_sub","type":"tool_result","content":"Linux","is_error":false}]},"tool_use_result":{"stdout":"Linux","stderr":"","interrupted":false,"isImage":false}}`
)

func slimEvent(t *testing.T, event string) map[string]any {
	t.Helper()
	line := slimToolResults(time.Unix(0, 0), json.RawMessage(event))
	if line == nil {
		t.Fatalf("no slim copy for %s", event)
	}
	var entry struct {
		Kind  string         `json:"kind"`
		Event map[string]any `json:"event"`
	}
	if err := json.Unmarshal(line, &entry); err != nil || entry.Kind != "event" {
		t.Fatalf("slim line %s: %v", line, err)
	}
	return entry.Event
}

func TestSlimToolResultsKeepWhatToolRowsShow(t *testing.T) {
	read := slimEvent(t, readResult)
	body, _ := json.Marshal(read)
	if strings.Contains(string(body), "SECRET FILE BODY") {
		t.Fatalf("file content sent: %s", body)
	}
	file := read["tool_use_result"].(map[string]any)["file"].(map[string]any)
	if file["numLines"] != float64(2) || file["filePath"] != "/workspace/notes.txt" {
		t.Fatalf("read counts lost: %v", file)
	}
	block := read["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if block["tool_use_id"] != "toolu_read" || block["content"] != "1\tline one\n2\tline two" {
		t.Fatalf("tool_result lost: %v", block)
	}

	edit := slimEvent(t, editResult)
	body, _ = json.Marshal(edit)
	if strings.Contains(string(body), "WHOLE ORIGINAL FILE") || strings.Contains(string(body), "oldString") {
		t.Fatalf("edit inputs repeated: %s", body)
	}
	if !strings.Contains(string(body), `"lines":["-print(1)","+print(2)"]`) {
		t.Fatalf("edit diff lost: %s", body)
	}

	fail := slimEvent(t, failResult)
	block = fail["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if block["is_error"] != true || fail["tool_use_result"] != "Error: Exit code 3\n2" {
		t.Fatalf("failure lost: %v", fail)
	}

	sub := slimEvent(t, subResult)
	if sub["parent_tool_use_id"] != "toolu_agent" {
		t.Fatalf("sub-agent link lost: %v", sub)
	}
	if _, ok := sub["tool_use_result"].(map[string]any)["stdout"]; ok {
		t.Fatalf("stdout repeats the tool_result text: %v", sub)
	}

	// Long output keeps its first lines and says how many were left out.
	lines := make([]string, 500)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	long, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_long", "content": strings.Join(lines, "\n")}}}})
	block = slimEvent(t, string(long))["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if n := strings.Count(block["content"].(string), "\n") + 1; n != slimLines || block["omitted_lines"] != float64(500-slimLines) {
		t.Fatalf("long output kept %d lines, omitted %v", n, block["omitted_lines"])
	}
	huge, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_huge", "content": strings.Repeat("界", 20000)}}}})
	block = slimEvent(t, string(huge))["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if text := block["content"].(string); len(text) > slimBytes+len("…") || !strings.HasSuffix(text, "…") {
		t.Fatalf("one long line not cut on a rune boundary: %d bytes", len(text))
	}

	// Nothing a tool row could claim: no copy at all.
	for _, ev := range []string{
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"no id"}]}}`,
		`{"type":"user","isReplay":true,"message":{"role":"user","content":"steer text"}}`,
		`{"type":"assistant","message":{"content":[]}}`,
	} {
		if line := slimToolResults(time.Unix(0, 0), json.RawMessage(ev)); line != nil {
			t.Fatalf("unexpected slim copy %s for %s", line, ev)
		}
	}
}

func TestHistorySendsSlimToolResults(t *testing.T) {
	s, sess := newTestServer(t)
	s.chat = newChatManager(s)
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	room := s.chat.room(sess.ID)
	for _, e := range []logEntry{
		{Kind: "user", Text: "read it", Turn: &chatTurnMetadata{ID: "t1", Model: "fixture"}},
		{Kind: "event", Event: json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_read","name":"Read","input":{"file_path":"/workspace/notes.txt"}}]}}`)},
		{Kind: "event", Event: json.RawMessage(readResult)},
		{Kind: "event", Event: json.RawMessage(`{"type":"result","subtype":"success"}`)},
	} {
		if err := room.appendLog(e); err != nil {
			t.Fatal(err)
		}
	}
	code, page := getHistory(t, s, sess, "")
	if code != http.StatusOK || len(page.Entries) != 4 {
		t.Fatalf("history %d: %d entries", code, len(page.Entries))
	}
	got := string(page.Entries[2].Event)
	if !strings.Contains(got, `"tool_use_id":"toolu_read"`) || strings.Contains(got, "SECRET FILE BODY") {
		t.Fatalf("tool result not slimmed: %s", got)
	}
}
