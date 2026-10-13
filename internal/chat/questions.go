package chat

import (
	"agentbox/internal/agent"
	"context"
	"sync"
	"time"
)

// QuestionTimeout is how long a question waits for the user before the model
// is told to go on with its own judgment. The turn's own time limit is paused
// meanwhile, so a slow answer never times the whole turn out.
var QuestionTimeout = 10 * time.Minute

// QuestionView is what history and live pages see of one AskUserQuestion
// call: first pending with its questions, then once more with how it ended.
type QuestionView struct {
	ID        string               `json:"id"`
	State     string               `json:"state"`
	Questions []agent.QuestionItem `json:"questions,omitempty"`
	Answers   map[string]string    `json:"answers,omitempty"`
	ExpiresAt *time.Time           `json:"expires_at,omitempty"`
}

// SteerView is a follow-up message handed to a running turn, recorded where
// the CLI took it ("applied") or at the turn's end if it never did ("dropped").
// ID is the follow-up's request ID.
type SteerView struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	State string `json:"state"`
}

// ask offers q to the answer API and waits for the first answer, the
// question's own time limit, or the CLI withdrawing it (an interrupt).
func ask(ctx context.Context, q agent.Question, r Runtime, clock *turnClock, record func(QuestionView)) agent.Answer {
	clock.pause()
	defer clock.resume()
	answers, withdraw := r.Question(q)
	defer withdraw()
	expires := time.Now().Add(QuestionTimeout)
	record(QuestionView{ID: q.ID, State: agent.QuestionPending, Questions: q.Items, ExpiresAt: &expires})
	timer := time.NewTimer(QuestionTimeout)
	defer timer.Stop()
	var a agent.Answer
	select {
	case a = <-answers:
	case <-timer.C:
		// An answer accepted just before withdrawal still counts.
		withdraw()
		select {
		case a = <-answers:
		default:
			a.State = agent.QuestionExpired
		}
	case <-ctx.Done():
		a.State = agent.QuestionCancelled
	}
	record(QuestionView{ID: q.ID, State: a.State, Answers: a.Answers})
	return a
}

// turnClock is a turn's time limit that stops while the turn waits for the
// user; nested questions pause it once.
type turnClock struct {
	mu     sync.Mutex
	timer  *time.Timer
	left   time.Duration
	since  time.Time
	paused int
}

func newTurnClock(limit time.Duration, expire func()) *turnClock {
	c := &turnClock{left: limit, since: time.Now()}
	c.timer = time.AfterFunc(limit, expire)
	return c
}

func (c *turnClock) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused++
	if c.paused == 1 && c.timer.Stop() {
		c.left -= time.Since(c.since)
	}
}

func (c *turnClock) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused--
	if c.paused == 0 {
		c.since = time.Now()
		c.timer.Reset(max(c.left, 0))
	}
}

func (c *turnClock) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timer.Stop()
}
