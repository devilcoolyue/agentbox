package agent

import (
	"encoding/json"
	"errors"
	"strings"
)

var ErrFinalOutput = errors.New("provider final answer unavailable or ambiguous")

// FinalOutput collects only complete provider-authored answers. It is scoped to
// one admitted turn; usage, transcript durability and exit status are checked by
// the caller before the answer is allowed to advance a collaboration.
type FinalOutput struct {
	agent, expectedSession, session string
	text, terminal, itemID          string
	max                             int
	bad, explicit                   bool
}

func NewFinalOutput(kind, expectedSession string, maxBytes int) *FinalOutput {
	return &FinalOutput{agent: kind, expectedSession: expectedSession, max: maxBytes,
		bad: (kind != "claude" && kind != "codex") || maxBytes < 1}
}

func (f *FinalOutput) Observe(line []byte) {
	var event struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		ThreadID  string `json:"thread_id"`
		Result    string `json:"result"`
		IsError   bool   `json:"is_error"`
		Item      struct {
			Type, Text, Phase, ID string
		} `json:"item"`
	}
	if json.Unmarshal(line, &event) != nil {
		return // Non-JSON output is never an answer.
	}
	id := event.SessionID
	if event.Type == "thread.started" {
		id = event.ThreadID
	}
	if id != "" {
		if (f.session != "" && f.session != id) || (f.expectedSession != "" && f.expectedSession != id) {
			f.bad = true
		}
		f.session = id
	}
	if f.agent == "claude" && event.Type == "result" {
		if f.terminal != "" || event.Subtype != "success" || event.IsError {
			f.bad = true
		}
		f.terminal = "completed"
		f.setText(event.Result)
		return
	}
	if f.agent != "codex" {
		return
	}
	switch event.Type {
	case "turn.completed", "turn.failed":
		if f.terminal != "" || event.Type != "turn.completed" {
			f.bad = true
		}
		f.terminal = event.Type
	case "item.started", "item.completed":
		if f.terminal != "" {
			f.bad = true
			return
		}
		if event.Item.Type != "agent_message" {
			// Without an explicit phase, only the last answer after all tools
			// is eligible. A commentary followed by tools cannot be a handoff.
			if f.explicit {
				f.bad = true
			}
			f.text = ""
			return
		}
		if event.Type != "item.completed" {
			return
		}
		if event.Item.Phase == "commentary" {
			if f.explicit {
				f.bad = true
			}
			f.text = ""
			return
		}
		if event.Item.Phase != "" && event.Item.Phase != "final_answer" {
			f.bad = true
			return
		}
		if f.explicit {
			// A replay of the exact same item is harmless; another final is
			// ambiguous and must not silently replace an earlier handoff.
			if f.itemID == "" || event.Item.ID != f.itemID || event.Item.Text != f.text {
				f.bad = true
			}
			return
		}
		f.explicit = event.Item.Phase == "final_answer"
		f.itemID = event.Item.ID
		f.setText(event.Item.Text)
	}
}

func (f *FinalOutput) setText(text string) {
	if len(text) > f.max {
		f.bad = true
		f.text = ""
		return
	}
	f.text = text
}

func (f *FinalOutput) Result() (string, error) {
	if f.bad || f.terminal == "" || f.session == "" || strings.TrimSpace(f.text) == "" {
		return "", ErrFinalOutput
	}
	return f.text, nil
}
