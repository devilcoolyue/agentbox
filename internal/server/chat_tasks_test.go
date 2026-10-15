package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func (f *receiptFixture) chatTasks(t *testing.T, user, thread string) (int, chatTasksView) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/sessions/"+f.sess.ID+"/chat/tasks?thread="+thread, nil)
	r.Header.Set("Authorization", "Bearer "+user)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	var view chatTasksView
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(err, w.Body.String())
		}
	}
	return w.Code, view
}

// The task list is read from the files Claude Code keeps per provider session
// (shape from 2.1.296 against a local mock API), found through the thread's
// last chat_session entry, with the container stopped.
func TestChatTasksReadsProviderTaskList(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, "", false)
	f.sess.Status = store.StatusStopped
	if err := f.s.store.Put(f.sess); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.s.chatsDir(f.sess), 0o755); err != nil {
		t.Fatal(err)
	}
	// Long tool output lines are skipped; the last chat_session wins.
	thread := strings.Join([]string{`{"kind":"chat_session","text":"old-session"}`, `{"kind":"event","event":{"type":"user","x":"` + strings.Repeat("a", 200<<10) + `"}}`,
		`{"kind":"chat_session","text":"22222222-2222-3333-4444-555555555555"}`, ``}, "\n")
	if err := os.WriteFile(f.s.threadPath(f.sess, "t1"), []byte(thread), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.s.homeDir(f.sess), ".claude", "tasks", "22222222-2222-3333-4444-555555555555")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"10.json":   `{"id":"10","subject":"Run tests","activeForm":"Running tests","status":"pending","blocks":[],"blockedBy":["2","3"]}`,
		"2.json":    `{"id":"2","subject":"Fix the bug","activeForm":"Fixing the bug","status":"in_progress","blocks":["10"],"blockedBy":[]}`,
		"3.json":    `{"id":"3","subject":"Read the config","description":"Read config.json","activeForm":"Reading the config","status":"completed","blocks":[],"blockedBy":[]}`,
		"4.json":    `{"id":"4","subject":"internal","status":"pending","metadata":{"_internal":true}}`,
		"5.json":    `{"id":"6","subject":"wrong id","status":"pending"}`,
		"7.json":    `{"id":"7","subject":"` + strings.Repeat("x", chatTaskFileBytes) + `","status":"pending"}`,
		"notes.txt": `{"id":"8","subject":"not a task","status":"pending"}`,
		".lock":     ``,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A link planted from inside the container is not followed.
	outside := filepath.Join(t.TempDir(), "secret.json")
	os.WriteFile(outside, []byte(`{"id":"9","subject":"host file","status":"pending"}`), 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "9.json")); err != nil {
		t.Fatal(err)
	}
	code, view := f.chatTasks(t, "alice", "t1")
	if code != 200 || len(view.Tasks) != 3 || view.Truncated {
		t.Fatal(code, view)
	}
	want := []chatTask{
		{ID: "2", Subject: "Fix the bug", ActiveForm: "Fixing the bug", Status: "in_progress"},
		{ID: "3", Subject: "Read the config", ActiveForm: "Reading the config", Status: "completed"},
		{ID: "10", Subject: "Run tests", ActiveForm: "Running tests", Status: "pending", BlockedBy: []string{"2"}},
	}
	for i, task := range want {
		got := view.Tasks[i]
		if got.ID != task.ID || got.Subject != task.Subject || got.ActiveForm != task.ActiveForm || got.Status != task.Status || strings.Join(got.BlockedBy, ",") != strings.Join(task.BlockedBy, ",") {
			t.Fatalf("task %d: %+v, want %+v", i, got, task)
		}
	}
	// A thread without a provider session, or one never run, has no list.
	if code, view := f.chatTasks(t, "alice", "t2"); code != 200 || len(view.Tasks) != 0 {
		t.Fatal(code, view)
	}
	if code, _ := f.chatTasks(t, "alice", "../t1"); code != 400 {
		t.Fatal("bad thread accepted", code)
	}
	if code, _ := f.chatTasks(t, "bob", "t1"); code == 200 {
		t.Fatal("another user's tasks read")
	}
	// The tasks directory itself swapped for a link is refused, not followed.
	os.Rename(dir, dir+".real")
	if err := os.Symlink(dir+".real", dir); err != nil {
		t.Fatal(err)
	}
	if code, view := f.chatTasks(t, "alice", "t1"); code == 200 && len(view.Tasks) != 0 {
		t.Fatal("followed a linked task directory", view)
	}
}

func TestChatTasksCodexHasNoTaskFiles(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentCodex, "", false)
	os.MkdirAll(f.s.chatsDir(f.sess), 0o755)
	os.WriteFile(f.s.threadPath(f.sess, "t1"), []byte(`{"kind":"chat_session","text":"abc"}`+"\n"), 0o644)
	if code, view := f.chatTasks(t, "alice", "t1"); code != 200 || len(view.Tasks) != 0 {
		t.Fatal(code, view)
	}
}

// A plan the model finished before a turn began is over, as the terminal
// resets it; one still being worked on, or touched after the turn began,
// stays. Times are the files' birth and last change.
func TestOpenPlanLeavesFinishedPlansOut(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 10, 15, 9, m, 0, 0, time.UTC) }
	task := func(id, status string, created, updated int) listedTask {
		return listedTask{task: chatTask{ID: id, Status: status}, created: at(created), updated: at(updated)}
	}
	ids := func(list []listedTask) string {
		out := []string{}
		for _, t := range list {
			out = append(out, t.task.ID)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		name   string
		tasks  []listedTask
		starts []int
		want   string
	}{
		// Turn 1 (minute 0) wrote the plan; nothing began after it.
		{"working", []listedTask{task("1", "completed", 1, 2), task("2", "pending", 1, 1)}, []int{0}, "1,2"},
		{"done-this-turn", []listedTask{task("1", "completed", 1, 2), task("2", "completed", 1, 3)}, []int{0}, "1,2"},
		// A step added mid-turn after the others were done is the same plan.
		{"added-mid-turn", []listedTask{task("1", "completed", 1, 2), task("2", "pending", 3, 3)}, []int{0}, "1,2"},
		// Turn 2 (minute 10) began with everything done: an ordinary answer shows nothing.
		{"next-turn", []listedTask{task("1", "completed", 1, 2), task("2", "completed", 1, 3)}, []int{0, 10}, ""},
		// ...and a new plan made in turn 2 shows alone.
		{"new-plan", []listedTask{task("1", "completed", 1, 2), task("2", "completed", 1, 3), task("3", "in_progress", 11, 12)}, []int{0, 10}, "3"},
		// Turn 3 (minute 20) carries on with the second plan: task 4 existed but was still open.
		{"carry-on", []listedTask{task("1", "completed", 1, 2), task("2", "completed", 1, 3), task("3", "completed", 11, 12), task("4", "completed", 11, 21)}, []int{0, 10, 20}, "3,4"},
		// Something still open when turn 2 began keeps the whole plan.
		{"unfinished", []listedTask{task("1", "completed", 1, 2), task("2", "pending", 1, 1)}, []int{0, 10, 20}, "1,2"},
		// Done tasks changed again after the turn began were not over yet.
		{"touched", []listedTask{task("1", "completed", 1, 2), task("2", "completed", 1, 11)}, []int{0, 10}, "1,2"},
	} {
		starts := []time.Time{}
		for _, m := range tc.starts {
			starts = append(starts, at(m))
		}
		if got := ids(openPlan(tc.tasks, starts)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The endpoint finds the turns in the thread (a long user message too) and
// the tasks' times on disk: the finished plan disappears once a new turn
// begins, and a plan made after that shows alone.
func TestChatTasksHideFinishedPlan(t *testing.T) {
	f := newAgentReceiptFixture(t, config.AgentClaude, "", false)
	os.MkdirAll(f.s.chatsDir(f.sess), 0o755)
	const psid = "33333333-2222-3333-4444-555555555555"
	dir := filepath.Join(f.s.homeDir(f.sess), ".claude", "tasks", psid)
	os.MkdirAll(dir, 0o755)
	user := func(ts time.Time, text string) string {
		line, _ := json.Marshal(logEntry{TS: ts, Kind: "user", Text: text})
		return string(line)
	}
	lines := []string{user(time.Now().Add(-time.Hour), "plan it"), `{"kind":"chat_session","text":"` + psid + `"}`}
	write := func() {
		t.Helper()
		if err := os.WriteFile(f.s.threadPath(f.sess, "t1"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	task := func(id, status string) {
		t.Helper()
		body := `{"id":"` + id + `","subject":"step ` + id + `","status":"` + status + `","blocks":[],"blockedBy":[]}`
		if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list := func() string {
		t.Helper()
		code, view := f.chatTasks(t, "alice", "t1")
		if code != 200 {
			t.Fatal(code)
		}
		out := []string{}
		for _, task := range view.Tasks {
			out = append(out, task.ID)
		}
		return strings.Join(out, ",")
	}
	task("1", "completed")
	task("2", "completed")
	write()
	if got := list(); got != "1,2" {
		t.Fatalf("finished in the last turn: %q", got)
	}
	time.Sleep(20 * time.Millisecond)
	lines = append(lines, user(time.Now(), strings.Repeat("长", 40<<10)))
	write()
	if got := list(); got != "" {
		t.Fatalf("finished plan shown in a later turn: %q", got)
	}
	time.Sleep(20 * time.Millisecond)
	task("3", "in_progress")
	if got := list(); got != "3" {
		t.Fatalf("new plan: %q", got)
	}
}
