package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Claude Code's stream-json control protocol, the one its Agent SDK speaks.
// With --input-format stream-json and --permission-prompt-tool stdio the CLI
// keeps reading stdin and asks its host before a tool that needs the user.
// Only AskUserQuestion reaches the user here; any other permission prompt is
// refused, as a plain -p turn would refuse it.

const AskUserQuestionTool = "AskUserQuestion"

// Question states as the history and the answer API see them.
const (
	QuestionPending   = "pending"
	QuestionAnswered  = "answered"
	QuestionSkipped   = "skipped"
	QuestionExpired   = "expired"
	QuestionCancelled = "cancelled"
)

// Bounds on what a model may put in front of the user and what comes back.
const (
	maxQuestions      = 8
	maxQuestionOpts   = 10
	maxQuestionText   = 2000
	maxAnswerRunes    = 4000
	maxAnswersTotalKB = 32
)

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type QuestionItem struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multi_select,omitempty"`
}

// Question is one AskUserQuestion call waiting for the user. ID is the
// tool_use_id, which history and the answer API share.
type Question struct {
	ID    string
	Items []QuestionItem
	// The CLI's own input is echoed back with the answers added.
	input     json.RawMessage
	requestID string
}

// Answer resolves a question. Answers maps each question's text to the
// user's choice; multiple choices are joined as Claude Code itself does.
type Answer struct {
	State   string
	Answers map[string]string
}

// Control is one control message the CLI wrote to stdout.
type Control struct {
	Type      string // control_request | control_cancel_request | control_response
	RequestID string
	Subtype   string
	ToolName  string
	ToolUseID string
	Input     json.RawMessage
}

// ParseClaudeControl recognizes a control message; any other line is an
// ordinary event for the chat stream.
func ParseClaudeControl(line []byte) (Control, bool) {
	if !bytes.Contains(line, []byte(`"control_`)) {
		return Control{}, false
	}
	var raw struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype   string          `json:"subtype"`
			ToolName  string          `json:"tool_name"`
			ToolUseID string          `json:"tool_use_id"`
			Input     json.RawMessage `json:"input"`
		} `json:"request"`
	}
	if json.Unmarshal(line, &raw) != nil || !strings.HasPrefix(raw.Type, "control_") {
		return Control{}, false
	}
	return Control{Type: raw.Type, RequestID: raw.RequestID, Subtype: raw.Request.Subtype,
		ToolName: raw.Request.ToolName, ToolUseID: raw.Request.ToolUseID, Input: raw.Request.Input}, true
}

// IsClaudeResult reports the turn's final result event, after which a
// one-turn process needs no more input.
func IsClaudeResult(line []byte) bool {
	if !bytes.Contains(line, []byte(`"result"`)) {
		return false
	}
	var probe struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(line, &probe) == nil && probe.Type == "result"
}

func ClaudeUserMessage(text string) []byte {
	raw, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	return append(raw, '\n')
}

// ClaudeSteer is a follow-up for the running turn: the CLI applies it after
// the current tool step, or as one more turn if the model already finished.
func ClaudeSteer(text string) []byte {
	raw, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}, "priority": "next"})
	return append(raw, '\n')
}

// IsClaudeReplay recognizes the CLI's echo of a user message it just took
// from stdin (--replay-user-messages); tool results are not echoes.
func IsClaudeReplay(line []byte) bool {
	if !bytes.Contains(line, []byte(`"isReplay"`)) {
		return false
	}
	var probe struct {
		Type     string `json:"type"`
		IsReplay bool   `json:"isReplay"`
	}
	return json.Unmarshal(line, &probe) == nil && probe.Type == "user" && probe.IsReplay
}

func ClaudeInterrupt(id string) []byte {
	raw, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": map[string]string{"subtype": "interrupt"}})
	return append(raw, '\n')
}

func controlResponse(requestID string, response map[string]any) []byte {
	raw, _ := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": requestID, "response": response}})
	return append(raw, '\n')
}

// ClaudeDeny refuses a permission request; the model sees message as the
// tool's error result and carries on.
func ClaudeDeny(requestID, message string) []byte {
	return controlResponse(requestID, map[string]any{"behavior": "deny", "message": message})
}

// ClaudeUnsupported answers a control request this host does not implement,
// so the CLI never waits on it.
func ClaudeUnsupported(requestID string) []byte {
	raw, _ := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": requestID, "error": "unsupported by agentbox"}})
	return append(raw, '\n')
}

// Instructions the model receives when a question ends without an answer.
const (
	questionSkippedMessage = "The user skipped this question without answering. Continue with your own best judgment and state the assumptions you made. If no safe assumption exists, stop and explain what you need the user to confirm."
	questionExpiredMessage = "The user did not answer within the time limit, so the question was skipped automatically. Continue with your own best judgment and state the assumptions you made. If no safe assumption exists, stop and explain what you need the user to confirm."
	// PermissionDeniedMessage refuses any other tool prompt; only questions reach the user.
	PermissionDeniedMessage = "This tool needs a permission that this chat cannot ask the user for, so it was denied."
)

// Reply turns an answer into the CLI's control response. A cancelled
// question gets none: the CLI withdrew it or the turn is ending.
func (q Question) Reply(a Answer) ([]byte, error) {
	switch a.State {
	case QuestionAnswered:
		var input map[string]any
		if err := json.Unmarshal(q.input, &input); err != nil {
			return nil, err
		}
		input["answers"] = a.Answers
		return controlResponse(q.requestID, map[string]any{"behavior": "allow", "updatedInput": input}), nil
	case QuestionSkipped:
		return ClaudeDeny(q.requestID, questionSkippedMessage), nil
	case QuestionExpired:
		return ClaudeDeny(q.requestID, questionExpiredMessage), nil
	}
	return nil, nil
}

var errQuestionInvalid = errors.New("question invalid")

// Tool use IDs name a question in URLs; providers vary their prefixes.
var toolUseIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ValidQuestionID reports whether id can name a question.
func ValidQuestionID(id string) bool { return toolUseIDRe.MatchString(id) }

// ClaudeQuestion reads an AskUserQuestion permission request. Text from the
// model is only bounded here; the web page escapes it when drawing the card.
func ClaudeQuestion(c Control) (Question, error) {
	if c.Type != "control_request" || c.Subtype != "can_use_tool" || c.ToolName != AskUserQuestionTool || c.RequestID == "" || !toolUseIDRe.MatchString(c.ToolUseID) {
		return Question{}, errQuestionInvalid
	}
	var in struct {
		Questions []struct {
			Question    string           `json:"question"`
			Header      string           `json:"header"`
			Options     []QuestionOption `json:"options"`
			MultiSelect bool             `json:"multiSelect"`
		} `json:"questions"`
	}
	if json.Unmarshal(c.Input, &in) != nil || len(in.Questions) == 0 || len(in.Questions) > maxQuestions {
		return Question{}, errQuestionInvalid
	}
	q := Question{ID: c.ToolUseID, input: c.Input, requestID: c.RequestID}
	seen := map[string]bool{}
	for _, item := range in.Questions {
		text := strings.TrimSpace(item.Question)
		if text == "" || seen[text] || len(item.Options) > maxQuestionOpts || !boundedText(item.Question, item.Header) {
			return Question{}, errQuestionInvalid
		}
		seen[text] = true
		for _, o := range item.Options {
			if strings.TrimSpace(o.Label) == "" || !boundedText(o.Label, o.Description) {
				return Question{}, errQuestionInvalid
			}
		}
		q.Items = append(q.Items, QuestionItem{Question: item.Question, Header: item.Header, Options: item.Options, MultiSelect: item.MultiSelect})
	}
	return q, nil
}

func boundedText(values ...string) bool {
	for _, v := range values {
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxQuestionText {
			return false
		}
	}
	return true
}

// CheckAnswers requires one non-empty answer for every question and nothing
// else. Free text beyond the offered options is allowed, as in the terminal.
func (q Question) CheckAnswers(answers map[string]string) error {
	if len(answers) != len(q.Items) {
		return fmt.Errorf("%w: answer every question", errQuestionInvalid)
	}
	total := 0
	for _, item := range q.Items {
		a, ok := answers[item.Question]
		if !ok || strings.TrimSpace(a) == "" || !utf8.ValidString(a) || utf8.RuneCountInString(a) > maxAnswerRunes {
			return fmt.Errorf("%w: answer missing or too long", errQuestionInvalid)
		}
		total += len(a)
	}
	if total > maxAnswersTotalKB<<10 {
		return fmt.Errorf("%w: answers too long", errQuestionInvalid)
	}
	return nil
}
