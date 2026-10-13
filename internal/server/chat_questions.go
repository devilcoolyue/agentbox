package server

import (
	"errors"
	"net/http"

	"agentbox/internal/agent"
	"agentbox/internal/store"
)

// Questions the model of a running turn asked the user (AskUserQuestion).
// They live only in the room: the CLI process holding the turn open is the
// only thing that can take the answer, so a restart drops them with the turn.
// History keeps a pending and a final record of each (chat.QuestionView).

type roomQuestion struct {
	q  agent.Question
	ch chan agent.Answer
}

var errQuestionClosed = errors.New("question closed")

func (r *chatRoom) offerQuestion(q agent.Question) (<-chan agent.Answer, func()) {
	entry := &roomQuestion{q: q, ch: make(chan agent.Answer, 1)}
	r.mu.Lock()
	if r.questions == nil {
		r.questions = map[string]*roomQuestion{}
	}
	r.questions[q.ID] = entry
	r.mu.Unlock()
	return entry.ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.questions[q.ID] == entry {
			delete(r.questions, q.ID)
		}
	}
}

// answerQuestion delivers the first answer from any page; a later one, or
// one after the question ended, gets errQuestionClosed.
func (r *chatRoom) answerQuestion(id string, a agent.Answer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.questions[id]
	if entry == nil {
		return errQuestionClosed
	}
	if a.State == agent.QuestionAnswered {
		if err := entry.q.CheckAnswers(a.Answers); err != nil {
			return store.ErrChatInvalid
		}
	}
	delete(r.questions, id)
	entry.ch <- a
	return nil
}

// handleChatQuestionAnswer answers or skips one pending question. The body
// carries either answers (question text -> answer) or skip:true.
func (s *Server) handleChatQuestionAnswer(w http.ResponseWriter, r *http.Request, sess store.Session) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("question")
	var body struct {
		Answers map[string]string `json:"answers"`
		Skip    bool              `json:"skip"`
	}
	if !agent.ValidQuestionID(id) || decodeChatRequest(w, r, &body) != nil || body.Skip == (len(body.Answers) > 0) {
		writeProblem(w, r, "chat.question", "invalid_request")
		return
	}
	a := agent.Answer{State: agent.QuestionAnswered, Answers: body.Answers}
	if body.Skip {
		a = agent.Answer{State: agent.QuestionSkipped}
	}
	switch err := s.chat.room(sess.ID).answerQuestion(id, a); {
	case errors.Is(err, errQuestionClosed):
		writeProblem(w, r, "chat.question", "question_closed")
	case err != nil:
		writeProblem(w, r, "chat.question", "invalid_request")
	default:
		writeJSON(w, 200, map[string]any{"version": 1, "state": a.State})
	}
}
