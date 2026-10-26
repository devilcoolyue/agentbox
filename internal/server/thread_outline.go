package server

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"agentbox/internal/store"
)

// answerKind tells which agent events carry a turn's answer text. A final
// answer (Claude's result, Codex's agent message) replaces whatever the turn
// showed before; interim assistant text only stands in until one arrives, so
// an interrupted turn still has something to show.
type answerKind uint8

const (
	noAnswer answerKind = iota
	interimAnswer
	finalAnswer
)

// threadMessage is one user message of a thread and the line holding its
// answer so far. The outline's excerpts are read on first request and kept,
// like the slim tool copies: the index lives as long as the file is unchanged
// apart from appends, and a later answer line replaces a stale excerpt.
type threadMessage struct {
	line  int
	reply int // line of the answer text, -1 while there is none
	final bool

	text     string
	textDone bool
	answer   string
	answerOf int // line answer was read from; 0 means not read (line 0 is never an answer)
}

func (m *threadMessage) observe(line int, kind answerKind) {
	if kind == finalAnswer || !m.final {
		m.reply, m.final = line, kind == finalAnswer
	}
}

// eventAnswer returns the answer text an agent event carries: Claude's result
// or assistant text blocks, or a completed Codex agent message.
func eventAnswer(typ string, result, message, item json.RawMessage) (string, answerKind) {
	switch typ {
	case "result":
		var text string
		if json.Unmarshal(result, &text) == nil {
			return text, finalAnswer
		}
	case "assistant":
		var m struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(message, &m) == nil {
			var parts []string
			for _, c := range m.Content {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					parts = append(parts, c.Text)
				}
			}
			return strings.Join(parts, "\n"), interimAnswer
		}
	case "item.completed":
		var it struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(item, &it) == nil && it.Type == "agent_message" {
			return it.Text, finalAnswer
		}
	}
	return "", noAnswer
}

// The page cuts the question to a few lines and the answer to its first
// sentence; these bound what it has to work with.
const (
	outlineTextRunes   = 200
	outlineAnswerRunes = 200
)

// outlineItem is one user message in GET outline. TS is the message's own
// timestamp, which the page uses to find the rendered message.
type outlineItem struct {
	Line   int       `json:"line"`
	TS     time.Time `json:"ts"`
	Text   string    `json:"text"`
	Answer string    `json:"answer,omitempty"`
}

func (x *threadIndex) outline(f *os.File) []outlineItem {
	items := make([]outlineItem, 0, len(x.messages))
	for i := range x.messages {
		m := &x.messages[i]
		item := outlineItem{Line: m.line}
		if e, err := x.entry(f, m.line); err == nil {
			item.TS = e.meta.TS
			if !m.textDone {
				m.text, m.textDone = truncRunes(strings.TrimSpace(e.meta.Text), outlineTextRunes), true
			}
		}
		item.Text = m.text
		if m.reply >= 0 && m.answerOf != m.reply {
			if e, err := x.entry(f, m.reply); err == nil {
				var ev struct {
					Type    string          `json:"type"`
					Result  json.RawMessage `json:"result"`
					Message json.RawMessage `json:"message"`
					Item    json.RawMessage `json:"item"`
				}
				_ = json.Unmarshal(e.meta.Event, &ev)
				text, _ := eventAnswer(ev.Type, ev.Result, ev.Message, ev.Item)
				m.answer, m.answerOf = truncRunes(strings.TrimSpace(text), outlineAnswerRunes), m.reply
			}
		}
		item.Answer = m.answer
		items = append(items, item)
	}
	return items
}

// handleOutline lists a thread's user messages with a short excerpt of each
// answer, for the navigation rail beside the conversation. Unlike history it
// covers the whole thread, so the page can jump to turns it has not loaded:
// line is the history cursor of the message (?thread=&from=). Without
// ?thread= it describes the active thread.
func (s *Server) handleOutline(w http.ResponseWriter, r *http.Request, sess store.Session) {
	room := s.chat.room(sess.ID)
	room.fileMu.Lock()
	defer room.fileMu.Unlock()
	if err := s.migrateThreads(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tid := r.URL.Query().Get("thread")
	if tid == "" {
		tid = s.activeThread(sess)
	} else if !threadIDRe.MatchString(tid) {
		writeProblem(w, r, "chat.history", "invalid_request")
		return
	}
	out := map[string]any{"thread_id": tid, "items": []outlineItem{}}
	if tid == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	x, f, err := s.threadIndex(s.threadPath(sess, tid))
	if os.IsNotExist(err) {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	out["items"] = x.outline(f)
	writeJSON(w, http.StatusOK, out)
}
