package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The index numbers and classifies lines exactly as parseEntries does, while
// growing with appends, rebuilding for a replaced or truncated file, and
// counting a final line that has no newline yet only until it gets one.
func TestThreadIndexFollowsParseEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	s := &Server{}
	check := func(label string) *threadIndex {
		t.Helper()
		x, f, err := s.threadIndex(path)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer f.Close()
		raw, _ := os.ReadFile(path)
		want := parseEntries(raw)
		if len(x.lines) != len(want) {
			t.Fatalf("%s: %d lines, parseEntries has %d", label, len(x.lines), len(want))
		}
		for i := range want {
			e, err := x.entry(f, i)
			if err != nil || !bytes.Equal(e.raw, want[i].raw) {
				t.Fatalf("%s: line %d = %q, want %q", label, i, e.raw, want[i].raw)
			}
			if x.lines[i].sent != historySent(&want[i]) || x.lines[i].user != (want[i].meta.Kind == "user") {
				t.Fatalf("%s: line %d flags %+v", label, i, x.lines[i])
			}
		}
		return x
	}
	write := func(flag int, text string) {
		t.Helper()
		f, err := os.OpenFile(path, flag|os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	user := func(text string) string {
		raw, _ := json.Marshal(logEntry{Kind: "user", Text: text})
		return string(raw) + "\n"
	}
	tool := `{"kind":"event","ts":"2026-10-01T00:00:00Z","event":{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + string(bytes.Repeat([]byte("x"), 4000)) + `"}]}}}` + "\n"
	write(os.O_TRUNC, user("first")+"\n  \r\n{broken\n"+tool+` {"kind":"status","state":"idle"}`+"\r\n "+user("第二条")+"\n")
	x := check("initial")
	if x.turns != 2 || x.first != "first" || !x.sent {
		t.Fatalf("meta = %+v", x.meta("t"))
	}
	consumed := x.consumed

	// An append is decoded from where the index stopped.
	write(os.O_APPEND, `{"kind":"title","text":"Named"}`+"\n"+`{"kind":"chat_session","text":"resume-1"}`+"\n")
	x = check("append")
	if x.title != "Named" || x.resume != "resume-1" || x.consumed <= consumed {
		t.Fatalf("after append: %+v consumed %d", x.meta("t"), x.consumed)
	}

	// A last line without its newline counts for this read only.
	write(os.O_APPEND, user("unterminated")[:len(user("unterminated"))-1])
	x = check("tail")
	if x.turns != 3 {
		t.Fatalf("tail not counted: %d turns", x.turns)
	}
	if cached := s.threadIndexes.get(path); cached.turns != 2 {
		t.Fatalf("tail cached: %d turns", cached.turns)
	}
	write(os.O_APPEND, "\n"+user("after"))
	if x = check("tail completed"); x.turns != 4 {
		t.Fatalf("completed tail: %d turns", x.turns)
	}

	// A rewritten file of any length is decoded again.
	write(os.O_TRUNC, user("only"))
	if x = check("truncated"); x.turns != 1 || x.first != "only" {
		t.Fatalf("truncated: %+v", x.meta("t"))
	}
	// Rewritten in place to the same length: the last line no longer matches.
	write(os.O_TRUNC, user("aaaa")+user("bbbb"))
	check("same length before")
	write(os.O_TRUNC, user("cccc")+user("dddd"))
	if x = check("same length after"); x.first != "cccc" {
		t.Fatalf("same-length rewrite kept the old index: %+v", x.meta("t"))
	}
	long := ""
	for i := range 20 {
		long += user(fmt.Sprintf("m%d", i))
	}
	os.Remove(path)
	write(os.O_TRUNC, long)
	if x = check("replaced"); x.turns != 20 || x.first != "m0" {
		t.Fatalf("replaced: %+v", x.meta("t"))
	}
	if _, _, err := s.threadIndex(filepath.Join(dir, "missing.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("missing file: %v", err)
	}
}

// A slim tool result counts toward a page's byte budget by its slim size, the
// same figure whether it was computed while picking the start or building it.
func TestThreadIndexPageSizesToolResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	tool := `{"kind":"event","ts":"2026-10-01T00:00:00Z","event":{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + string(bytes.Repeat([]byte("x"), 40000)) + `"}]}}}`
	text := `{"kind":"user","text":"q"}` + "\n" + tool + "\n" + `{"kind":"status","state":"idle"}` + "\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	x, f, err := s.threadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries := parseEntries([]byte(text))
	want := len(historyTool(&entries[1]))
	if got := x.pageSize(f, 1); got != want || want >= 40000 {
		t.Fatalf("slim size %d, want %d", got, want)
	}
	page, err := x.page(f, 0, len(x.lines))
	if err != nil || len(page) != 3 || len(page[1]) != want || string(page[0]) != `{"kind":"user","text":"q"}` || string(page[2]) != `{"kind":"status","state":"idle"}` {
		t.Fatalf("page = %d lines, err %v", len(page), err)
	}
}
