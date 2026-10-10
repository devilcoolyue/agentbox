package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"agentbox/internal/store"
)

type gitOperationContextKey struct{}

var errGitCancelled = errors.New("Git operation cancelled by user")
var gitRequestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,80}$`)

type gitOperationRegistry struct {
	mu     sync.Mutex
	active map[string]*gitLiveOperation
}
type gitLiveOperation struct {
	id        string
	actor     string
	operation string
	session   string
	started   time.Time
	cancel    context.CancelCauseFunc
	cancelled atomic.Bool
	read      atomic.Int64
	written   atomic.Int64
	mu        sync.Mutex
	phase     string
	record    int64
	// Latest Git --progress meter: whitelisted stage key and Git's own counts.
	stage      string
	percent    int
	stageDone  int64
	stageTotal int64
}
type gitOperationView struct {
	RequestID       string `json:"request_id"`
	ID              int64  `json:"id"`
	Operation       string `json:"operation"`
	SessionID       string `json:"session_id"`
	StartedAt       string `json:"started_at"`
	ElapsedMS       int64  `json:"elapsed_ms"`
	Phase           string `json:"phase"`
	ReceivedBytes   int64  `json:"received_bytes"`
	SentBytes       int64  `json:"sent_bytes"`
	CancelRequested bool   `json:"cancel_requested"`
	// Stage is empty until Git reports a progress meter; older servers omit it.
	Stage        string `json:"stage,omitempty"`
	StagePercent int    `json:"stage_percent"`
	StageDone    int64  `json:"stage_done"`
	StageTotal   int64  `json:"stage_total"`
}

func (op *gitLiveOperation) view() gitOperationView {
	op.mu.Lock()
	defer op.mu.Unlock()
	return gitOperationView{RequestID: op.id, ID: op.record, Operation: op.operation, SessionID: op.session, StartedAt: op.started.UTC().Format(time.RFC3339Nano), ElapsedMS: time.Since(op.started).Milliseconds(), Phase: op.phase, ReceivedBytes: op.read.Load(), SentBytes: op.written.Load(), CancelRequested: op.cancelled.Load(),
		Stage: op.stage, StagePercent: op.percent, StageDone: op.stageDone, StageTotal: op.stageTotal}
}

// Git's --progress meters, local ("Receiving objects") or relayed from the
// remote over the side band ("remote: Compressing objects"). Only these stage
// names are recognised: other stderr text, including anything a remote sends,
// never reaches the browser.
var gitProgressLine = regexp.MustCompile(`^(remote: +)?([A-Z][a-z]+ [a-z]+): +(\d{1,3})% \((\d{1,15})/(\d{1,15})\)`)
var gitProgressStages = map[string]string{
	"Counting objects": "counting", "Compressing objects": "compressing",
	"Receiving objects": "receiving", "Writing objects": "writing",
	"Resolving deltas": "resolving", "Updating files": "updating",
	"Checking connectivity": "connectivity",
}

func parseGitProgress(line string) (stage string, percent int, done, total int64, ok bool) {
	m := gitProgressLine.FindStringSubmatch(line)
	if m == nil {
		return "", 0, 0, 0, false
	}
	stage, ok = gitProgressStages[m[2]]
	if !ok {
		return "", 0, 0, 0, false
	}
	if m[1] != "" {
		stage = "remote_" + stage
	}
	percent, _ = strconv.Atoi(m[3])
	done, _ = strconv.ParseInt(m[4], 10, 64)
	total, _ = strconv.ParseInt(m[5], 10, 64)
	if percent > 100 || done > total {
		return "", 0, 0, 0, false
	}
	return stage, percent, done, total, true
}

// gitProgress records Git's stderr progress for the live operation, if any.
func (op *gitLiveOperation) gitProgress(line string) {
	stage, percent, done, total, ok := parseGitProgress(line)
	if !ok {
		return
	}
	op.mu.Lock()
	op.stage, op.percent, op.stageDone, op.stageTotal = stage, percent, done, total
	op.mu.Unlock()
}
func gitLive(ctx context.Context) *gitLiveOperation {
	op, _ := ctx.Value(gitOperationContextKey{}).(*gitLiveOperation)
	return op
}
func gitPhase(ctx context.Context, phase string) {
	if op := gitLive(ctx); op != nil {
		op.mu.Lock()
		op.phase = phase
		op.mu.Unlock()
	}
}
func (s *Server) beginGitOperation(ctx context.Context, actor, session, repo, connection, operation, target string) (int64, error) {
	id, err := s.store.BeginGitOperation(actor, session, repo, connection, operation, target)
	if err == nil {
		if op := gitLive(ctx); op != nil {
			op.mu.Lock()
			op.record = id
			op.mu.Unlock()
		}
	}
	return id, err
}
func (s *Server) finishGitOperation(ctx context.Context, id int64, result string) {
	if ctx.Err() != nil && result != "success" && result != "success_binding_failed" {
		result = "cancelled_unknown"
	}
	if err := s.store.FinishGitOperation(id, result); err != nil {
		log.Printf("git operation %d: could not persist final status: %v", id, err)
	}
}

// Keep requests synchronous, but expose an independently authenticated status
// and cancellation handle. No detached jobs survive the server lifecycle.
func (s *Server) gitOperation(name string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := reqUser(r).Name
		id := r.Header.Get("X-Git-Request-ID")
		if id == "" {
			id = store.NewID() + store.NewID()
		}
		if !gitRequestIDPattern.MatchString(id) {
			writeErr(w, 400, "Git 操作标识无效")
			return
		}
		ctx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)
		op := &gitLiveOperation{id: id, actor: actor, session: r.PathValue("id"), operation: name, started: time.Now(), cancel: cancel, phase: "preparing"}
		key := actor + "\x00" + id
		s.gitOperations.mu.Lock()
		if s.gitOperations.active == nil {
			s.gitOperations.active = map[string]*gitLiveOperation{}
		}
		n := 0
		for _, v := range s.gitOperations.active {
			if v.actor == actor {
				n++
			}
		}
		if _, exists := s.gitOperations.active[key]; exists || n >= 4 {
			s.gitOperations.mu.Unlock()
			writeErr(w, 409, "Git 操作正在进行或已达并发上限，请等待或取消现有操作")
			return
		}
		s.gitOperations.active[key] = op
		s.gitOperations.mu.Unlock()
		defer func() { s.gitOperations.mu.Lock(); delete(s.gitOperations.active, key); s.gitOperations.mu.Unlock() }()
		w.Header().Set("X-Git-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, gitOperationContextKey{}, op)))
	})
}
func (s *Server) handleGitOperations(w http.ResponseWriter, r *http.Request) {
	var before int64
	if value := r.URL.Query().Get("before"); value != "" {
		var err error
		before, err = strconv.ParseInt(value, 10, 64)
		if err != nil || before < 0 {
			writeErr(w, 400, "分页参数无效")
			return
		}
	}
	actor := reqUser(r).Name
	rows, err := s.store.GitOperations(actor, before, 51)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	var next int64
	if len(rows) > 50 {
		rows = rows[:50]
		next = rows[49].ID
	}
	active := []gitOperationView{}
	s.gitOperations.mu.Lock()
	for _, op := range s.gitOperations.active {
		if op.actor == actor {
			active = append(active, op.view())
		}
	}
	s.gitOperations.mu.Unlock()
	sort.Slice(active, func(i, j int) bool { return active[i].StartedAt > active[j].StartedAt })
	writeJSON(w, 200, map[string]any{"rows": rows, "active": active, "next_before": next})
}
func (s *Server) handleGitOperationCancel(w http.ResponseWriter, r *http.Request) {
	key := reqUser(r).Name + "\x00" + r.PathValue("operation")
	s.gitOperations.mu.Lock()
	op := s.gitOperations.active[key]
	if op == nil {
		s.gitOperations.mu.Unlock()
		writeErr(w, 404, "操作已经结束或不存在，请刷新记录核实结果")
		return
	}
	op.cancelled.Store(true)
	op.cancel(errGitCancelled)
	s.gitOperations.mu.Unlock()
	writeJSON(w, 202, map[string]bool{"cancel_requested": true})
}
