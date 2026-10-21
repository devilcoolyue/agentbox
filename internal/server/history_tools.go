package server

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// Claude tool results ("user" events) feed the CLI and are often whole file
// contents, so history never sends them as written. The web draws each tool
// call the way Claude Code's terminal does (features/chat/claude-tools.ts) and
// needs a few facts from the result: whether it failed, the start of its
// output, line counts, the diff of an edit, a fetch's status. A page carries
// that slim copy instead; it is bounded and does not count as a rendered entry.

const (
	slimLines    = 80      // lines of output kept from the start
	slimBytes    = 8 << 10 // and at most this many bytes of them
	slimPatch    = 400     // diff lines kept across an edit's hunks
	slimList     = 100     // file names, search hits
	slimFieldMax = 1 << 10 // small structured fields beyond this are dropped
)

// slimTurKeep are tool_use_result fields kept as they are: counts, flags,
// statuses and other short values the tool rows show.
var slimTurKeep = map[string]bool{
	"type": true, "filePath": true, "numLines": true, "startLine": true, "totalLines": true,
	"userModified": true, "replaceAll": true, "interrupted": true, "isImage": true,
	"backgroundTaskId": true, "noOutputExpected": true, "query": true, "durationSeconds": true,
	"searchCount": true, "bytes": true, "code": true, "codeText": true, "durationMs": true,
	"url": true, "status": true, "agentId": true, "agentType": true, "totalDurationMs": true,
	"totalTokens": true, "totalToolUseCount": true, "mode": true, "numFiles": true,
	"numMatches": true, "success": true, "taskId": true, "statusChange": true, "matches": true,
	"task": true, "truncated": true,
}

// clipText keeps the first lines of s within slimLines and slimBytes and
// reports how many lines were left out.
func clipText(s string) (string, int) {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "", 0
	}
	lines := strings.Split(s, "\n")
	kept, size := 0, 0
	for kept < len(lines) && kept < slimLines {
		n := len(lines[kept]) + 1
		if size+n > slimBytes {
			break
		}
		size += n
		kept++
	}
	if kept == len(lines) {
		return s, 0
	}
	if kept == 0 { // one very long first line: cut it on a rune boundary
		cut := slimBytes
		for cut > 0 && !utf8.RuneStart(lines[0][cut]) {
			cut--
		}
		return lines[0][:cut] + "…", len(lines) - 1
	}
	return strings.Join(lines[:kept], "\n"), len(lines) - kept
}

func clipString(s string) string {
	out, omitted := clipText(s)
	if omitted > 0 && !strings.HasSuffix(out, "…") {
		out += "\n…"
	}
	return out
}

// resultText joins the text blocks of a tool_result's content.
func resultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(content, &blocks)
	parts := []string{}
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func slimPatchHunks(raw json.RawMessage) any {
	var hunks []map[string]json.RawMessage
	if json.Unmarshal(raw, &hunks) != nil {
		return nil
	}
	left := slimPatch
	out := []map[string]any{}
	for _, h := range hunks {
		if left <= 0 {
			break
		}
		var lines []string
		_ = json.Unmarshal(h["lines"], &lines)
		if len(lines) > left {
			lines = lines[:left]
		}
		for i, l := range lines {
			if len(l) > slimFieldMax {
				lines[i] = l[:slimFieldMax]
			}
		}
		left -= len(lines)
		hunk := map[string]any{"lines": lines}
		for _, k := range []string{"oldStart", "oldLines", "newStart", "newLines"} {
			if v, ok := h[k]; ok {
				hunk[k] = v
			}
		}
		out = append(out, hunk)
	}
	return out
}

// slimWebResults keeps titles and links of a WebSearch result.
func slimWebResults(raw json.RawMessage) any {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := []any{}
	for _, item := range items {
		var hit struct {
			ToolUseID string `json:"tool_use_id"`
			Content   []struct {
				Title string `json:"title"`
				URL   string `json:"url"`
			} `json:"content"`
		}
		if json.Unmarshal(item, &hit) != nil {
			continue // commentary strings: the answer repeats them
		}
		if len(hit.Content) > slimList {
			hit.Content = hit.Content[:slimList]
		}
		out = append(out, hit)
	}
	return out
}

// slimToolUseResult keeps what the tool rows read from tool_use_result.
func slimToolUseResult(raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return clipString(s)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range m {
		if slimTurKeep[k] && len(v) <= slimFieldMax {
			out[k] = v
		}
	}
	if f, ok := m["file"]; ok {
		var file map[string]json.RawMessage
		if json.Unmarshal(f, &file) == nil {
			keep := map[string]json.RawMessage{}
			for _, k := range []string{"filePath", "numLines", "startLine", "totalLines"} {
				if v, ok := file[k]; ok {
					keep[k] = v
				}
			}
			out["file"] = keep
		}
	}
	if p, ok := m["structuredPatch"]; ok {
		if hunks := slimPatchHunks(p); hunks != nil {
			out["structuredPatch"] = hunks
		}
	}
	if f, ok := m["filenames"]; ok {
		var names []string
		if json.Unmarshal(f, &names) == nil {
			if len(names) > slimList {
				names = names[:slimList]
			}
			out["filenames"] = names
		}
	}
	if r, ok := m["results"]; ok {
		if hits := slimWebResults(r); hits != nil {
			out["results"] = hits
		}
	}
	if r, ok := m["result"]; ok {
		var text string
		if json.Unmarshal(r, &text) == nil {
			out["result"] = clipString(text)
		}
	}
	// content: a sub-agent's reply (text blocks) or Grep's matching lines; for
	// Write it is the whole file again, which the tool_use already carries.
	if c, ok := m["content"]; ok && m["filePath"] == nil {
		var text string
		if json.Unmarshal(c, &text) == nil {
			out["content"] = clipString(text)
		} else if reply := resultText(c); reply != "" {
			out["content"] = []map[string]string{{"type": "text", "text": clipString(reply)}}
		}
	}
	return out
}

// slimToolResults turns a logged Claude "user" event into the copy a history
// page sends, or nil when nothing in it belongs to a tool row (no tool_result
// with an id: the row could not be found anyway).
func slimToolResults(ts time.Time, event json.RawMessage) json.RawMessage {
	var ev struct {
		Type          string          `json:"type"`
		ParentToolUse *string         `json:"parent_tool_use_id"`
		ToolUseResult json.RawMessage `json:"tool_use_result"`
		Message       struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(event, &ev) != nil || ev.Type != "user" {
		return nil
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(ev.Message.Content, &blocks) != nil {
		return nil
	}
	type slimBlock struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		IsError   bool   `json:"is_error,omitempty"`
		Content   string `json:"content"`
		Omitted   int    `json:"omitted_lines,omitempty"`
	}
	kept := []slimBlock{}
	for _, b := range blocks {
		if b.Type != "tool_result" || b.ToolUseID == "" {
			continue
		}
		text, omitted := clipText(resultText(b.Content))
		kept = append(kept, slimBlock{Type: b.Type, ToolUseID: b.ToolUseID, IsError: b.IsError, Content: text, Omitted: omitted})
	}
	if len(kept) == 0 {
		return nil
	}
	slim := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": kept}}
	if ev.ParentToolUse != nil {
		slim["parent_tool_use_id"] = *ev.ParentToolUse
	}
	// The CLI attaches tool_use_result only when the event holds one result.
	if len(kept) == 1 && len(ev.ToolUseResult) > 0 && string(ev.ToolUseResult) != "null" {
		if tur := slimToolUseResult(ev.ToolUseResult); tur != nil {
			slim["tool_use_result"] = tur
		}
	}
	body, err := json.Marshal(slim)
	if err != nil {
		return nil
	}
	line, err := json.Marshal(logEntry{TS: ts, Kind: "event", Event: body})
	if err != nil {
		return nil
	}
	return line
}
