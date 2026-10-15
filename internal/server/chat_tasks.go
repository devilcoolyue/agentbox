package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

// The task list Claude Code keeps while it works (TaskCreate / TaskUpdate;
// 2.1 turns it on by default only for the models it lists, Claude 5 runs
// without one unless the user opts in) lives in the session home: one JSON
// file per task under .claude/tasks/<provider session id>/. The chat page
// shows it the way the terminal does. It is read from the host, so it
// survives a reload and a stopped container; the tool results that carry
// task ids never reach the page's history anyway.
const (
	chatTasksMax      = 200
	chatTaskFileBytes = 64 << 10
)

type chatTask struct {
	ID         string   `json:"id"`
	Subject    string   `json:"subject"`
	ActiveForm string   `json:"active_form,omitempty"`
	Status     string   `json:"status"`
	BlockedBy  []string `json:"blocked_by,omitempty"`
}

type chatTasksView struct {
	Tasks []chatTask `json:"tasks"`
	// More task files than chatTasksMax: the list shows the first ones.
	Truncated bool `json:"truncated,omitempty"`
}

var (
	providerSessionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	taskFileRe        = regexp.MustCompile(`^([0-9]{1,9})\.json$`)
)

func (s *Server) handleChatTasks(w http.ResponseWriter, r *http.Request, sess store.Session) {
	tid := r.URL.Query().Get("thread")
	if !threadIDRe.MatchString(tid) {
		writeErr(w, http.StatusBadRequest, "invalid thread")
		return
	}
	view := chatTasksView{Tasks: []chatTask{}}
	if sess.Agent == config.AgentClaude {
		if id, starts := s.threadTaskContext(sess, tid); providerSessionRe.MatchString(id) {
			var err error
			if view, err = s.readChatTasks(sess, id, starts); err != nil {
				writeFileOpErr(w, err)
				return
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, view)
}

// userEntryHead is the start of a user message line in a thread: Entry's
// ts and kind come first, so it is found without parsing (or reading all
// of) a long message.
var userEntryHead = regexp.MustCompile(`^\{"ts":"([^"]{1,64})","kind":"user"`)

// threadTaskContext is the thread's provider conversation and when each of
// its turns began, in one pass over the thread.
func (s *Server) threadTaskContext(sess store.Session, tid string) (resume string, starts []time.Time) {
	s.streamThread(sess, tid, func(line []byte, whole bool) {
		if m := userEntryHead.FindSubmatch(line); m != nil {
			if ts, err := time.Parse(time.RFC3339Nano, string(m[1])); err == nil {
				starts = append(starts, ts)
			}
		} else if whole && bytes.Contains(line, []byte(`"chat_session"`)) {
			var e logEntry
			if json.Unmarshal(line, &e) == nil && e.Kind == "chat_session" && e.Text != "" {
				resume = e.Text
			}
		}
	})
	return resume, starts
}

type listedTask struct {
	n       int
	task    chatTask
	created time.Time // the file's birth, or its last change where unknown
	updated time.Time
}

// openPlan leaves out plans that were over before a turn began. The terminal
// resets the list once every task is done; -p never does, so the files stay
// and a later turn would show the old list again. A turn start finishes the
// plan when every task that existed then (and is not already finished) was
// completed and untouched since; one changed after the turn began was still
// open, which keeps its plan. tasks are in id (creation) order.
func openPlan(tasks []listedTask, starts []time.Time) []listedTask {
	done := make([]bool, len(tasks))
	for _, start := range starts {
		existed, finished := false, true
		for i, t := range tasks {
			if done[i] || !t.created.Before(start) {
				continue
			}
			existed = true
			if t.task.Status != "completed" || !t.updated.Before(start) {
				finished = false
				break
			}
		}
		if !existed || !finished {
			continue
		}
		for i, t := range tasks {
			if t.created.Before(start) {
				done[i] = true
			}
		}
	}
	open := tasks[:0:0]
	for i, t := range tasks {
		if !done[i] {
			open = append(open, t)
		}
	}
	return open
}

// readChatTasks reads the list the way the CLI shows it: finished plans and
// tasks marked internal are left out, and blocked_by only names tasks still
// open. The directory belongs to the container, so every file goes through
// the pinned root and only regular files of bounded size are read.
func (s *Server) readChatTasks(sess store.Session, id string, starts []time.Time) (chatTasksView, error) {
	view := chatTasksView{Tasks: []chatTask{}}
	root, err := s.openDataDir(filepath.Join(s.homeDir(sess), ".claude", "tasks", id))
	if errors.Is(err, fs.ErrNotExist) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	defer root.Close()
	entries, err := root.ReadDirLimit(".", 4*chatTasksMax)
	if err != nil {
		return view, err
	}
	var tasks []listedTask
	for _, e := range entries {
		m := taskFileRe.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		raw, created, updated, err := readTaskFile(root, e.Name())
		if err != nil {
			continue // replaced or rewritten mid-read: the next refresh shows it
		}
		var t struct {
			ID         string         `json:"id"`
			Subject    string         `json:"subject"`
			ActiveForm string         `json:"activeForm"`
			Status     string         `json:"status"`
			BlockedBy  []string       `json:"blockedBy"`
			Metadata   map[string]any `json:"metadata"`
		}
		if json.Unmarshal(raw, &t) != nil || t.ID != m[1] || t.Subject == "" {
			continue
		}
		if internal, _ := t.Metadata["_internal"].(bool); internal {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		tasks = append(tasks, listedTask{n, chatTask{ID: t.ID, Subject: t.Subject, ActiveForm: t.ActiveForm, Status: t.Status, BlockedBy: t.BlockedBy}, created, updated})
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].n < tasks[j].n })
	tasks = openPlan(tasks, starts)
	if len(tasks) > chatTasksMax {
		tasks, view.Truncated = tasks[:chatTasksMax], true
	}
	open := map[string]bool{}
	for _, t := range tasks {
		if t.task.Status != "completed" {
			open[t.task.ID] = true
		}
	}
	for _, t := range tasks {
		var blocked []string
		for _, b := range t.task.BlockedBy {
			if open[b] {
				blocked = append(blocked, b)
			}
		}
		t.task.BlockedBy = blocked
		view.Tasks = append(view.Tasks, t.task)
	}
	return view, nil
}

// readTaskFile reads one task file through the pinned root, with when it was
// created and last changed.
func readTaskFile(root *safefs.Root, name string) (raw []byte, created, updated time.Time, err error) {
	f, err := root.OpenFile(name)
	if err != nil {
		return nil, created, updated, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, created, updated, err
	}
	updated = info.ModTime()
	created, ok := fileBirth(f)
	if !ok || created.After(updated) {
		created = updated
	}
	raw, err = io.ReadAll(io.LimitReader(f, chatTaskFileBytes+1))
	if err == nil && len(raw) > chatTaskFileBytes {
		err = errors.New("task file too large")
	}
	return raw, created, updated, err
}
