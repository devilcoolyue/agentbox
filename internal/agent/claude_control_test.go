package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func askRequest(input string) Control {
	line := `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":` + input + `,"tool_use_id":"toolu_1"}}`
	c, ok := ParseClaudeControl([]byte(line))
	if !ok {
		panic("not a control line")
	}
	return c
}

func TestClaudeControlRecognizesOnlyTopLevelControl(t *testing.T) {
	for _, line := range []string{
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"{\"type\":\"control_request\"}"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"control_request"}]}}`,
		`not json "control_request"`,
	} {
		if _, ok := ParseClaudeControl([]byte(line)); ok {
			t.Fatal("ordinary event taken for control", line)
		}
	}
	if !IsClaudeResult([]byte(`{"type":"result","subtype":"success"}`)) || IsClaudeResult([]byte(`{"type":"assistant","text":"\"type\":\"result\""}`)) {
		t.Fatal("result detection")
	}
	if c, ok := ParseClaudeControl([]byte(`{"type":"control_cancel_request","request_id":"r1"}`)); !ok || c.Type != "control_cancel_request" || c.RequestID != "r1" {
		t.Fatal(c, ok)
	}
}

func TestClaudeQuestionBoundsAndReplies(t *testing.T) {
	q, err := ClaudeQuestion(askRequest(`{"questions":[{"question":"Pick","header":"H","options":[{"label":"a"},{"label":"b","description":"d"}],"multiSelect":true}],"metadata":{"source":"x"}}`))
	if err != nil || q.ID != "toolu_1" || len(q.Items) != 1 || !q.Items[0].MultiSelect || q.Items[0].Options[1].Description != "d" {
		t.Fatal(q, err)
	}
	for _, bad := range []string{
		`{"questions":[]}`,
		`{"questions":[{"question":" ","options":[]}]}`,
		`{"questions":[{"question":"same","options":[]},{"question":"same","options":[]}]}`,
		`{"questions":[{"question":"q","options":[{"label":""}]}]}`,
		`{"questions":[{"question":"` + strings.Repeat("x", 2001) + `","options":[]}]}`,
		`null`,
	} {
		if _, err := ClaudeQuestion(askRequest(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	if q.CheckAnswers(map[string]string{"Pick": "a, b"}) != nil || q.CheckAnswers(map[string]string{}) == nil ||
		q.CheckAnswers(map[string]string{"Pick": "a", "Other": "b"}) == nil || q.CheckAnswers(map[string]string{"Pick": strings.Repeat("y", 4001)}) == nil {
		t.Fatal("answer checks")
	}
	raw, err := q.Reply(Answer{State: QuestionAnswered, Answers: map[string]string{"Pick": "free text"}})
	var reply struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior     string         `json:"behavior"`
				UpdatedInput map[string]any `json:"updatedInput"`
				Message      string         `json:"message"`
			} `json:"response"`
		} `json:"response"`
	}
	if err != nil || json.Unmarshal(raw, &reply) != nil || reply.Response.RequestID != "r1" || reply.Response.Response.Behavior != "allow" ||
		reply.Response.Response.UpdatedInput["metadata"] == nil || reply.Response.Response.UpdatedInput["answers"].(map[string]any)["Pick"] != "free text" {
		t.Fatal("allow keeps the CLI's own input and adds answers", string(raw), err)
	}
	for state, want := range map[string]string{QuestionSkipped: "skipped", QuestionExpired: "time limit"} {
		raw, _ := q.Reply(Answer{State: state})
		if json.Unmarshal(raw, &reply) != nil || reply.Response.Response.Behavior != "deny" || !strings.Contains(reply.Response.Response.Message, want) {
			t.Fatal(state, string(raw))
		}
	}
	if raw, err := q.Reply(Answer{State: QuestionCancelled}); raw != nil || err != nil {
		t.Fatal("withdrawn question answered", string(raw))
	}
	if ValidQuestionID("../x") || ValidQuestionID("") || !ValidQuestionID("toolu_bdrk_01AbC-9") {
		t.Fatal("question id")
	}
}
