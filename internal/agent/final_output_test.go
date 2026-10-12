package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFinalOutputRequiresCompleteProviderResult(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			f := NewFinalOutput(kind, "thread-1", 1024)
			for _, raw := range []string{`{"type":"system","session_id":"thread-1"}`, `{"type":"user","result":"fake"}`, `{"type":"item.completed","item":{"type":"command_execution","text":"fake"}}`, `{"type":"stream_event","event":{"delta":{"text":"fake"}}}`} {
				f.Observe([]byte(raw))
			}
			if _, err := f.Result(); err == nil {
				t.Fatal("progress/tool output completed turn")
			}
			if kind == "claude" {
				f.Observe([]byte(`{"type":"result","subtype":"success","session_id":"thread-1","result":"real"}`))
			} else {
				f.Observe([]byte(`{"type":"item.completed","item":{"id":"m1","type":"agent_message","phase":"final_answer","text":"real"}}`))
				if _, err := f.Result(); err == nil {
					t.Fatal("missing provider terminal accepted")
				}
				f.Observe([]byte(`{"type":"turn.completed"}`))
			}
			text, err := f.Result()
			if err != nil || text != "real" {
				t.Fatal(text, err)
			}
		})
	}
}

func TestFinalOutputRejectsWrongSessionFailureAndOversize(t *testing.T) {
	for _, events := range [][]string{
		{`{"type":"result","subtype":"success","session_id":"wrong","result":"text"}`},
		{`{"type":"result","subtype":"error_during_execution","session_id":"expected","is_error":true,"result":"text"}`},
		{`{"type":"result","subtype":"success","session_id":"expected","result":"text"}`, `{"type":"result","subtype":"success","session_id":"expected","result":"other"}`},
		{`{"type":"result","subtype":"success","result":"text"}`},
	} {
		f := NewFinalOutput("claude", "expected", 32)
		for _, e := range events {
			f.Observe([]byte(e))
		}
		if _, err := f.Result(); err == nil {
			t.Fatal("ambiguous result accepted")
		}
	}
	f := NewFinalOutput("claude", "", 32)
	raw, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "session_id": "s", "result": strings.Repeat("x", 33)})
	f.Observe(raw)
	if _, err := f.Result(); err == nil {
		t.Fatal("oversize final accepted")
	}
}

func TestCodexFinalOutputIgnoresCommentaryAndInvalidatesPreToolText(t *testing.T) {
	for _, phase := range []string{"commentary", ""} {
		f := NewFinalOutput("codex", "", 1024)
		f.Observe([]byte(`{"type":"thread.started","thread_id":"T1"}`))
		raw, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": "fake handoff", "phase": phase}})
		f.Observe(raw)
		f.Observe([]byte(`{"type":"item.started","item":{"type":"command_execution"}}`))
		f.Observe([]byte(`{"type":"turn.completed"}`))
		if _, err := f.Result(); err == nil {
			t.Fatal("pre-tool text became final")
		}
	}
	f := NewFinalOutput("codex", "", 1024)
	for _, e := range []string{`{"type":"thread.started","thread_id":"T1"}`, `{"type":"item.completed","item":{"type":"agent_message","text":"legacy final"}}`, `{"type":"turn.completed"}`} {
		f.Observe([]byte(e))
	}
	if text, err := f.Result(); err != nil || text != "legacy final" {
		t.Fatal(text, err)
	}
}
