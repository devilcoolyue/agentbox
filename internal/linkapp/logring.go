package linkapp

import (
	"fmt"
	"sync"
	"time"
)

// logRing keeps the last N log lines in memory for the panel to poll. Each
// line carries a monotonic sequence number so the UI can ask for "everything
// after 412" instead of re-rendering the whole buffer.
type logRing struct {
	mu    sync.Mutex
	lines []LogLine
	next  int64
	max   int
	// mirror, when set, also receives every line — used by the headless CLI
	// mode, whose "panel" is the terminal.
	mirror func(string)
}

// LogLine is one entry in the panel's log view.
type LogLine struct {
	Seq  int64  `json:"seq"`
	Time int64  `json:"time"` // unix millis
	Text string `json:"text"`
}

func newLogRing(max int) *logRing { return &logRing{max: max} }

func (r *logRing) Printf(format string, args ...any) {
	text := fmt.Sprintf(format, args...)

	r.mu.Lock()
	r.next++
	r.lines = append(r.lines, LogLine{Seq: r.next, Time: time.Now().UnixMilli(), Text: text})
	if len(r.lines) > r.max {
		r.lines = append(r.lines[:0:0], r.lines[len(r.lines)-r.max:]...)
	}
	mirror := r.mirror
	r.mu.Unlock()

	// Outside the lock: the mirror writes to a terminal and must not be able to
	// stall other loggers.
	if mirror != nil {
		mirror(text)
	}
}

// Since returns the lines newer than seq, plus the latest sequence number.
// Passing 0 asks for the whole buffer.
func (r *logRing) Since(seq int64) ([]LogLine, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []LogLine{}
	for _, l := range r.lines {
		if l.Seq > seq {
			out = append(out, l)
		}
	}
	return out, r.next
}
