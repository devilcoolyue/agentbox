package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGitOperationProgressCancellationAndIsolation(t *testing.T) {
	s, _ := newTestServer(t)
	ready := make(chan struct{})
	done := make(chan struct{})
	handler := s.gitOperation("fetch", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.beginGitOperation(r.Context(), "alice", "s1", "project", "c1", "fetch", "origin")
		if err != nil {
			t.Error(err)
			close(ready)
			return
		}
		gitPhase(r.Context(), "transferring")
		gitLive(r.Context()).read.Add(1024)
		close(ready)
		<-r.Context().Done()
		if context.Cause(r.Context()) != errGitCancelled {
			t.Error("cancellation cause lost")
		}
		s.finishGitOperation(r.Context(), id, "failed")
		writeErr(w, 409, "cancelled")
	}))
	request := accessRequest("alice", "POST", "/git/fetch", "")
	request.Header.Set("X-Git-Request-ID", "synthetic-operation-123")
	go func() { defer close(done); handler.ServeHTTP(httptest.NewRecorder(), request) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not start")
	}
	read := func(user string) struct {
		Rows   []any              `json:"rows"`
		Active []gitOperationView `json:"active"`
	} {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleGitOperations(w, accessRequest(user, "GET", "/git/operations", ""))
		if w.Code != 200 {
			t.Fatalf("list: %s", w.Body.String())
		}
		var out struct {
			Rows   []any              `json:"rows"`
			Active []gitOperationView `json:"active"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if view := read("alice"); len(view.Active) != 1 || view.Active[0].ReceivedBytes != 1024 || view.Active[0].Phase != "transferring" {
		t.Fatalf("progress: %+v", view)
	}
	for _, user := range []string{"bob", "root"} {
		if view := read(user); len(view.Active) != 0 || len(view.Rows) != 0 {
			t.Fatal("operation leaked across users")
		}
		r := accessRequest(user, "POST", "/cancel", "")
		r.SetPathValue("operation", "synthetic-operation-123")
		w := httptest.NewRecorder()
		s.handleGitOperationCancel(w, r)
		if w.Code != 404 {
			t.Fatal("another user cancelled operation")
		}
	}
	r := accessRequest("alice", "POST", "/cancel", "")
	r.SetPathValue("operation", "synthetic-operation-123")
	w := httptest.NewRecorder()
	s.handleGitOperationCancel(w, r)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("operation not cancelled")
	}
	if len(read("alice").Active) != 0 {
		t.Fatal("completed operation remains active")
	}
	rows, err := s.store.GitOperations("alice", 0, 10)
	if err != nil || len(rows) != 1 || rows[0].Result != "cancelled_unknown" || rows[0].FinishedAt == "" {
		t.Fatalf("audit: %+v %v", rows, err)
	}
}

func TestParseGitProgress(t *testing.T) {
	cases := []struct {
		line        string
		stage       string
		percent     int
		done, total int64
		ok          bool
	}{
		{"Receiving objects:  45% (450/1000), 1.20 MiB | 600.00 KiB/s", "receiving", 45, 450, 1000, true},
		{"remote: Compressing objects: 100% (12/12), done.", "remote_compressing", 100, 12, 12, true},
		{"remote:   Counting objects:   7% (7/100)", "remote_counting", 7, 7, 100, true},
		{"Writing objects:  62% (31/50), 2.00 KiB | 2.00 MiB/s", "writing", 62, 31, 50, true},
		{"Resolving deltas: 100% (3/3), done.", "resolving", 100, 3, 3, true},
		// Unknown stages and arbitrary remote text are ignored, not relayed.
		{"remote: Visit https://example.invalid 50% (1/2)", "", 0, 0, 0, false},
		{"remote: Hacking files:  50% (1/2)", "", 0, 0, 0, false},
		{"Enumerating objects: 1234, done.", "", 0, 0, 0, false},
		{"Receiving objects: 900% (9/1)", "", 0, 0, 0, false},
		{"fatal: could not read from remote", "", 0, 0, 0, false},
	}
	for _, c := range cases {
		stage, percent, done, total, ok := parseGitProgress(c.line)
		if stage != c.stage || percent != c.percent || done != c.done || total != c.total || ok != c.ok {
			t.Errorf("%q => %q %d %d/%d %v", c.line, stage, percent, done, total, ok)
		}
	}
	op := &gitLiveOperation{started: time.Now()}
	op.gitProgress("Receiving objects:  45% (450/1000)")
	op.gitProgress("remote: something unrelated")
	if v := op.view(); v.Stage != "receiving" || v.StagePercent != 45 || v.StageDone != 450 || v.StageTotal != 1000 {
		t.Fatalf("view: %+v", v)
	}
}
