package delegation

import (
	"encoding/json"
	"strings"
)

// Report collects a worker's final answer from its event stream. Claude Code
// ends with a result event; Codex's last agent_message is its answer. Text
// blocks seen along the way are a fallback when the final event is missing.
type Report struct {
	final, last string
}

func (r *Report) Observe(line []byte) {
	var e struct {
		Type    string `json:"type"`
		Result  string `json:"result"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	switch e.Type {
	case "result":
		if strings.TrimSpace(e.Result) != "" {
			r.final = e.Result
		}
	case "assistant":
		var parts []string
		for _, c := range e.Message.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) > 0 {
			r.last = strings.Join(parts, "\n")
		}
	case "item.completed":
		if e.Item.Type == "agent_message" && strings.TrimSpace(e.Item.Text) != "" {
			r.final = e.Item.Text
		}
	}
}

// Text returns the bounded report.
func (r *Report) Text() string {
	text := r.final
	if text == "" {
		text = r.last
	}
	return truncate(strings.TrimSpace(text), MaxReportBytes)
}
