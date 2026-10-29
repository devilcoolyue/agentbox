package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"agentbox/internal/agent"
	"agentbox/internal/chat"
	"agentbox/internal/config"
	"agentbox/internal/delegation"
	"agentbox/internal/store"
)

// titleTimeout bounds the one-shot model call that names a new thread.
const titleTimeout = 90 * time.Second

// --- chat manager: one room per session, broadcasting to all open tabs.
// Each thread of a session has its own turn slot (chatTurn); up to
// maxParallelChats() of them run at once, each its own CLI process. ---

type chatManager struct {
	srv   *Server
	mu    sync.Mutex
	rooms map[string]*chatRoom
}

func newChatManager(s *Server) *chatManager {
	return &chatManager{srv: s, rooms: map[string]*chatRoom{}}
}

// maxParallelChats bounds the turns one workspace runs at the same time
// (max_parallel_chats): every running thread is a CLI process in the same
// container, sharing its memory.
func (r *chatRoom) maxParallelChats() int {
	if r.srv == nil || r.srv.cfg == nil {
		return config.DefaultMaxParallelChats
	}
	return r.srv.cfg.GetMaxParallelChats()
}

// parallelLimitChanged tells open pages the new limit and, when it grew,
// starts queued requests that were waiting for a free slot.
func (m *chatManager) parallelLimitChanged() {
	n := m.srv.cfg.GetMaxParallelChats()
	m.mu.Lock()
	rooms := make([]*chatRoom, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	m.mu.Unlock()
	for _, r := range rooms {
		r.broadcast(map[string]any{"type": "chat_limit", "max_parallel": n})
		r.kickQueue()
	}
}

func (m *chatManager) room(sessID string) *chatRoom {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rooms[sessID]; ok {
		return r
	}
	r := &chatRoom{srv: m.srv, sessID: sessID, conns: map[*connWriter]bool{}}
	m.rooms[sessID] = r
	return r
}

type connWriter struct {
	mu sync.Mutex
	c  *websocket.Conn
}

func (cw *connWriter) send(v any) error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.c.SetWriteDeadline(time.Now().Add(wsWriteWait))
	return cw.c.WriteJSON(v)
}

type chatRoom struct {
	srv    *Server
	sessID string

	mu        sync.Mutex
	conns     map[*connWriter]bool
	turns     map[string]*chatTurn // thread → its turn slot
	exclusive bool                 // an account switch holds the whole workspace: no turn starts
	submitMu  sync.Mutex           // receipt admission, orphan recovery and explicit review

	// fileMu guards the session's on-disk chat state: thread transcripts,
	// the active-thread pointer and the one-time legacy migration.
	fileMu sync.Mutex

	helpMu        sync.Mutex // see cliHelp
	helpContainer string
	help          string

	commandsMu        sync.Mutex // see slashCommands
	commandsContainer string
	commandsAt        time.Time
	commands          []agent.SlashCommand

	questions map[string]*roomQuestion // under mu; see chat_questions.go
}

// chatTurn is one thread's turn slot. At most one turn runs in a thread; the
// thread's transcript, resume id and CLI pid file belong to it alone, so other
// threads of the workspace run beside it. Fields are guarded by room.mu.
type chatTurn struct {
	room   *chatRoom
	thread string

	running         bool
	held            bool      // a thread operation (delete) owns the slot without running a turn
	startedAt       time.Time // when the running turn was admitted; drives the page's elapsed counter
	phase           string    // what the running turn waits on before its CLI runs (chatPhaseStarting), or ""
	turnDone        chan struct{}
	stop            func() // 当前回合的优雅中断（app-server 回合设置）；nil 时走 SIGINT
	requestID       string
	userInterrupted bool
	steer           chat.Steer // set while the running turn takes follow-ups
}

// turn returns the thread's slot, creating it on first use.
func (r *chatRoom) turn(thread string) *chatTurn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turnLocked(thread)
}

func (r *chatRoom) turnLocked(thread string) *chatTurn {
	if r.turns == nil {
		r.turns = map[string]*chatTurn{}
	}
	t := r.turns[thread]
	if t == nil {
		t = &chatTurn{room: r, thread: thread}
		r.turns[thread] = t
	}
	return t
}

// runningLocked lists the running turns; the caller holds mu.
func (r *chatRoom) runningLocked() []*chatTurn {
	var out []*chatTurn
	for _, t := range r.turns {
		if t.running {
			out = append(out, t)
		}
	}
	return out
}

// anyRunning reports whether any thread of the workspace runs a turn.
func (r *chatRoom) anyRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runningLocked()) > 0
}

// threadRunning reports whether the thread runs a turn.
func (r *chatRoom) threadRunning(thread string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.turns[thread]
	return t != nil && t.running
}

// runningStatus describes every running turn for a page that joins: how long
// each has run (not a start time, so a page with a wrong clock still counts
// right) and what it waits on.
func (r *chatRoom) runningStatus() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]any{}
	for _, t := range r.runningLocked() {
		st := map[string]any{"elapsed_ms": time.Since(t.startedAt).Milliseconds()}
		if t.phase != "" {
			st["phase"] = t.phase
		}
		out[t.thread] = st
	}
	return out
}

// beginExclusive holds the whole workspace (an account switch): it fails while
// any thread runs or is held, and no turn starts until endExclusive.
func (r *chatRoom) beginExclusive() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exclusive {
		return errChatBusy
	}
	for _, t := range r.turns {
		if t.running || t.held {
			return errChatBusy
		}
	}
	r.exclusive = true
	return nil
}

func (r *chatRoom) endExclusive() {
	r.mu.Lock()
	r.exclusive = false
	r.mu.Unlock()
	r.kickQueue()
}

// hold takes the thread's slot for a thread operation (delete) without running
// a turn; it fails while the thread runs.
func (t *chatTurn) hold() error {
	r := t.room
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.running || t.held || r.exclusive {
		return errChatBusy
	}
	t.held = true
	return nil
}

func (t *chatTurn) unhold() {
	r := t.room
	r.mu.Lock()
	t.held = false
	r.mu.Unlock()
	r.kickQueue()
}

// tagged marks an event with the thread it belongs to, so pages showing
// another thread file it under that thread instead of rendering it.
type threadTagged struct {
	thread string
	value  any
}

func (v threadTagged) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(v.value)
	if err != nil || len(raw) < 2 || raw[0] != '{' {
		return raw, err
	}
	tag, _ := json.Marshal(v.thread)
	out := append([]byte(`{"thread":`), tag...)
	if string(raw) != "{}" {
		out = append(out, ',')
	}
	return append(out, raw[1:]...), nil
}

// broadcast sends a thread's event to every page of the workspace.
func (t *chatTurn) broadcast(v any) { t.room.broadcast(threadTagged{t.thread, v}) }

func (r *chatRoom) broadcast(v any) {
	r.mu.Lock()
	conns := make([]*connWriter, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()
	for _, c := range conns {
		if err := c.send(v); err != nil {
			r.detach(c)
		}
	}
}

func (r *chatRoom) attach(c *connWriter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[c] = true
}

func (r *chatRoom) detach(c *connWriter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, c)
}

func (r *chatRoom) state() string {
	if r.anyRunning() {
		return "running"
	}
	return "idle"
}

// chatPhaseStarting is the running turn starting its stopped workspace, which
// takes a while before the user's message is even recorded.
const chatPhaseStarting = "starting"

// setPhase records what the running turn waits on and tells the pages, so a
// turn that first starts its workspace says so instead of looking stuck.
func (t *chatTurn) setPhase(phase string) {
	r := t.room
	r.mu.Lock()
	if !t.running || t.phase == phase {
		r.mu.Unlock()
		return
	}
	t.phase = phase
	r.mu.Unlock()
	t.broadcast(map[string]any{"type": "status", "state": "running", "phase": phase})
}

// tryBegin marks the thread busy; returns false if it cannot start a turn now.
func (t *chatTurn) tryBegin() bool {
	return t.begin() == nil
}

var errChatBusy = errors.New("chat room busy")

// errChatParallel is the workspace already running maxParallelChats() threads.
var errChatParallel = errors.New("too many threads running")

func (t *chatTurn) begin() error {
	r := t.room
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.running || t.held || r.exclusive {
		return errChatBusy
	}
	if len(r.runningLocked()) >= r.maxParallelChats() {
		return errChatParallel
	}
	if r.srv != nil && r.srv.store != nil {
		if _, err := r.srv.store.PendingChatRequest(r.sessID, t.thread); err == nil {
			return store.ErrChatPending
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	t.running, t.startedAt, t.phase = true, time.Now(), ""
	t.requestID, t.userInterrupted = "", false
	t.turnDone = make(chan struct{})
	return nil
}

// end releases the thread and lets queued follow-ups (of any thread) start.
func (t *chatTurn) end() {
	t.release()
	t.room.kickQueue()
}

func (t *chatTurn) release() {
	r := t.room
	r.mu.Lock()
	defer r.mu.Unlock()
	t.running = false
	t.requestID, t.phase = "", ""
	if t.turnDone != nil {
		close(t.turnDone)
		t.turnDone = nil
	}
}

// --- persistence: every chat event is appended to the active thread's
// transcript (chats/<threadID>.jsonl, see threads.go) so the UI can restore
// the conversation after the session (or server) restarts ---

// Immutable request settings, saved only after validation. An empty effort is
// CLI inheritance, not a claim about the provider's effective reasoning level.
type chatTurnMetadata = chat.TurnMetadata
type logEntry = chat.Entry

// History and live clients see the same timestamp and settings snapshot.
func (t *chatTurn) recordChat(e logEntry, messageType string, retryText string) error {
	e.TS = time.Now()
	err := t.room.appendLog(t.thread, e)
	t.broadcast(struct {
		logEntry
		Type      string `json:"type"`
		RetryText string `json:"retry_text,omitempty"`
	}{e, messageType, retryText})
	return err
}

// appendLog appends to the thread's transcript; every turn writes its own
// thread, whichever one the pages show. An empty thread is the active one.
func (r *chatRoom) appendLog(thread string, e logEntry) error {
	sess, ok := r.srv.store.Get(r.sessID)
	if !ok {
		return errSessionGone
	}
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	if err := r.srv.migrateThreads(sess); err != nil {
		log.Printf("chat threads %s: %v", r.sessID, err)
		return err
	}
	tid := thread
	if tid == "" {
		var err error
		if tid, err = r.srv.ensureActiveThread(sess); err != nil {
			log.Printf("chat threads %s: %v", r.sessID, err)
			return err
		}
	}
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(r.srv.threadPath(sess, tid), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("chat log %s: %v", r.sessID, err)
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

// --- websocket endpoint ---

func (s *Server) handleChatWS(w http.ResponseWriter, r *http.Request, sess store.Session) {
	upgrader := s.upgrader
	reported := false
	upgrader.Error = func(w http.ResponseWriter, req *http.Request, status int, _ error) {
		reported = true
		p := operationProblem(req.Context(), "chat.connect", "websocket_failed")
		w.Header().Set("X-Agentbox-Operation-ID", p.OperationID)
		writeJSON(w, status, p)
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		if !reported {
			operationProblem(r.Context(), "chat.connect", "websocket_failed")
		}
		return
	}
	release, ok := s.track(func() { _ = conn.Close() })
	if !ok {
		return
	}
	defer release()
	room := s.chat.room(sess.ID)
	cw := &connWriter{c: conn}
	room.attach(cw)
	defer func() {
		room.detach(cw)
		conn.Close()
	}()

	// A page joining mid-turn gets how long each running thread's turn has
	// run, not a start time, so its counter is right even when its clock is
	// off. Pages from before parallel threads read the top level, which
	// describes the active thread.
	running := room.runningStatus()
	status := map[string]any{"type": "status", "state": "idle", "threads": running, "max_parallel": room.maxParallelChats()}
	room.fileMu.Lock()
	active := s.activeThread(sess)
	room.fileMu.Unlock()
	if st, ok := running[active].(map[string]any); ok {
		status["state"] = "running"
		for k, v := range st {
			status[k] = v
		}
	}
	_ = cw.send(status)
	if _, paused := s.quotaPause(sess); paused {
		_ = cw.send(room.quotaPauseMessage())
	}

	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetReadLimit(4 << 20)
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	stop := make(chan struct{})
	pingDone := make(chan struct{})
	defer func() { close(stop); <-pingDone }()
	go func() {
		defer close(pingDone)
		t := time.NewTicker(wsPingPeriod)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait))
			}
		}
	}()

	for {
		var msg struct {
			Type          string `json:"type"`
			Text          string `json:"text"`
			Model         string `json:"model"`
			Effort        string `json:"effort"`
			EffortControl string `json:"effort_control"`
			Thread        string `json:"thread"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				operationProblem(r.Context(), "chat.connection", "websocket_failed")
			}
			return
		}
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		switch msg.Type {
		case "user_message":
			if msg.Text == "" {
				continue
			}
			// A fresh operation per message, independent of the socket lifetime.
			turnContext := operationContext(s.workContext())
			reject := func(code string) {
				p := operationProblem(turnContext, "chat.admission", code)
				_ = cw.send(struct {
					Type string `json:"type"`
					apiProblem
				}{"error", p})
			}
			// 额度拦在回合开始前：这里是唯一会花钱的入口，放进来就收不住了。
			if why := s.quotaBlock(sess.User); why != "" {
				reject("quota_exhausted")
				continue
			}
			if _, err := s.sessionAccount(sess); err != nil {
				reject(classifyProblem(err, "account_unavailable"))
				continue
			}
			if _, paused := s.quotaPaused(sess.ID); paused {
				reject(chatRequestErrorCode(errChatPaused))
				continue
			}
			// This legacy path names no thread: it runs in the active one.
			tid, err := room.activeThreadEnsured()
			if err != nil {
				reject(classifyProblem(err, "internal_error"))
				continue
			}
			turn := room.turn(tid)
			if err := turn.begin(); err != nil {
				reject(chatRequestErrorCode(err))
				continue
			}
			if !s.spawn(func() { turn.runTurnContext(turnContext, msg.Text, msg.Model, msg.Effort, msg.EffortControl) }) {
				turn.end()
				reject("server_stopping")
				return
			}
		case "interrupt":
			thread := msg.Thread
			if thread == "" || !threadIDRe.MatchString(thread) {
				room.fileMu.Lock()
				thread = s.activeThread(sess)
				room.fileMu.Unlock()
			}
			if thread != "" {
				room.turn(thread).interrupt()
			}
		}
	}
}

// runTurn executes one headless agent turn: start container if needed, feed
// the prompt on stdin, stream JSONL events to every attached client, record
// the provider session id for resume.
func (t *chatTurn) runTurn(text, model, effort string, controls ...string) {
	t.runTurnContext(operationContext(t.room.srv.workContext()), text, model, effort, controls...)
}

func (t *chatTurn) runTurnContext(parent context.Context, text, model, effort string, controls ...string) {
	t.runTurnObserved(parent, text, model, effort, nil, controls...)
}

// activeThreadEnsured is the active thread, created when there is none yet.
func (r *chatRoom) activeThreadEnsured() (string, error) {
	sess, ok := r.srv.store.Get(r.sessID)
	if !ok {
		return "", errSessionGone
	}
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	if err := r.srv.migrateThreads(sess); err != nil {
		return "", err
	}
	return r.srv.ensureActiveThread(sess)
}

func (t *chatTurn) runTurnObserved(parent context.Context, text, model, effort string, receipt *chatReceiptTurn, controls ...string) {
	r := t.room
	defer t.end()
	var observer chat.Observation
	if receipt != nil {
		observer = receipt
	}
	control := ""
	if len(controls) > 0 {
		control = controls[0]
	}
	service := chat.Service{Runtime: chatRuntime{t}, Usage: r.srv.usageService()}
	in := chat.Input{SessionID: r.sessID, Text: text, Model: model, Effort: effort, Control: control, Timeout: r.srv.turnTimeout()}
	if receipt != nil && receipt.receipt.Request != nil {
		in.Suggestions = receipt.receipt.Request.Suggestions
		in.Questions = receipt.receipt.Request.Questions
		in.Tasks = receipt.receipt.Request.Tasks
	}
	if receipt != nil && receipt.receipt.Request != nil && receipt.receipt.Request.Delegation != nil {
		// The grant lives exactly as long as this turn; closing it cancels any
		// running worker before the room is released.
		grant, err := r.srv.openDelegation(t, receipt.owner, receipt.receipt)
		if err != nil {
			receipt.Fail(err, classifyProblem(err, "delegation_unavailable"))
			receipt.Finish()
			return
		}
		if grant != nil {
			defer grant.close()
			in.Extras, in.Timeout, in.Preface = grant.extras, r.srv.delegationTurnTimeout(), delegation.TurnReminder()
		}
	}
	// The thread's own pid file: a stop signal must reach this thread's CLI,
	// never another thread's running beside it.
	in.Extras.PidFile = agent.ChatPidFile(t.thread)
	service.Run(parent, in, observer)
}

func (t *chatTurn) setStop(f func()) {
	t.room.mu.Lock()
	t.stop = f
	t.room.mu.Unlock()
}

func (t *chatTurn) setSteer(f chat.Steer) {
	t.room.mu.Lock()
	t.steer = f
	t.room.mu.Unlock()
}

// preTurnTitleState reports the turn's thread and whether this turn is its
// first user message with no title yet — the trigger for title generation.
func (t *chatTurn) preTurnTitleState(sess store.Session) (tid string, firstTurn bool) {
	r := t.room
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	if err := r.srv.migrateThreads(sess); err != nil {
		return "", false
	}
	tid = t.thread
	_, userTurns, hasTitle := r.srv.threadTitleSource(sess, tid)
	// 尚无标题、且这是线程最初的消息（含首条失败后的一次重试）时才触发，
	// 既保证只据开场消息生成，也不会每轮都白跑一次模型。
	return tid, !hasTitle && userTurns <= 1
}

// generateTitle asks the agent to summarize the thread's opening message into a
// short title, stores it, and broadcasts it. Best-effort: any failure leaves
// the thread on its first-message fallback title. Runs in its own goroutine.
func (r *chatRoom) generateTitle(tid, firstMsg string) {
	s := r.srv
	sess, ok := s.store.Get(r.sessID)
	if !ok || sess.ContainerID == "" {
		return
	}
	// 起标题是服务端自己发起的额外一趟消耗。用户的额度刚被上一个回合花光时
	// 就别再替他花了——标题只是锦上添花，欠着的账不是。
	if s.quotaBlock(sess.User) != "" {
		return
	}
	acct, err := s.sessionAccount(sess)
	if err != nil {
		return
	}
	adapter, err := agent.Lookup(sess.Agent)
	if err != nil {
		return
	}
	cmd, err := adapter.Title()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.workContext(), titleTimeout)
	defer cancel()
	titleStart := time.Now()
	env, err := s.execEnv(sess)
	if err != nil {
		return
	}
	titleTally := s.usageService().NewTally()
	out, err := s.dock.ExecCapture(ctx, sess.ContainerID, cmd, env, titlePrompt(firstMsg))
	if err != nil {
		log.Printf("gen title %s: %v", r.sessID, err)
		return
	}
	raw, usage := adapter.ParseTitle(out)
	// 先记账再管标题：钱已经花掉了，标题为空或被并发抢先都不改变这一点。
	if len(usage) > 0 {
		titleTally.Observe(titleUsageBase(sess, acct, tid), usage)
		r.flushUsage(&titleTally, time.Since(titleStart))
	}
	title := sanitizeTitle(raw)
	if title == "" {
		return
	}
	r.fileMu.Lock()
	// 二次确认没有并发写入标题（另一个页面/回合），避免重复
	if _, _, hasTitle := s.threadTitleSource(sess, tid); hasTitle {
		r.fileMu.Unlock()
		return
	}
	err = s.appendThreadEntry(sess, tid, logEntry{Kind: "title", Text: title})
	r.fileMu.Unlock()
	if err != nil {
		log.Printf("save title %s: %v", r.sessID, err)
		return
	}
	r.broadcast(map[string]any{"type": "thread_title", "id": tid, "title": title})
}

// titleUsageBase attributes a title run's usage. Codex's turn.completed names
// no model: its title command passes no -m, so it ran on the model in the
// session's config.toml — the session default SeedDefaultModel wrote there.
// Recording it prices the row by that model instead of the agent fallback.
// Claude's model comes from the result's modelUsage.
func titleUsageBase(sess store.Session, acct config.Account, tid string) store.UsageEvent {
	base := store.UsageEvent{
		User: sess.User, SessionID: sess.ID, ThreadID: tid, TurnID: store.NewID(),
		Agent: sess.Agent, AccountID: sess.AccountID, Kind: store.UsageKindTitle,
	}
	if sess.Agent == config.AgentCodex {
		base.Model = acct.ResolveModel(sess.DefaultModel)
	}
	return base
}

// titlePrompt wraps the opening message with instructions to produce a concise
// thread title. Only the head of a long message is needed to capture intent.
func titlePrompt(msg string) string {
	msg = strings.TrimSpace(msg)
	if rs := []rune(msg); len(rs) > 800 {
		msg = string(rs[:800])
	}
	return "为下面这条对话的开场消息起一个简短标题，用于在历史对话列表中标识它。" +
		"要求：概括核心意图；使用与原消息相同的语言；不超过16个汉字（英文不超过约6个词）；" +
		"只输出标题本身，不要引号、不要句末标点、不要任何解释或前后缀。\n\n开场消息：\n" + msg
}

// sanitizeTitle reduces raw model output to a single clean title line.
func sanitizeTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i] // 只取首行，忽略偶发的多行解释
	}
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'`“”‘’「」《》【】 　")
	s = strings.TrimSpace(s)
	if rs := []rune(s); len(rs) > 40 {
		s = string(rs[:40]) + "…"
	}
	return s
}

func (t *chatTurn) interrupt() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = t.interruptRequest(ctx, "", true)
}

func (t *chatTurn) interruptContext(ctx context.Context) {
	_ = t.interruptRequest(ctx, "", false)
}

func (t *chatTurn) interruptRequest(ctx context.Context, id string, user bool) error {
	// app-server 回合注册了协议内的优雅中断，优先用它
	r := t.room
	r.mu.Lock()
	defer r.mu.Unlock()
	if !t.running || id != "" && t.requestID != id {
		return store.ErrChatConflict
	}
	if user && t.userInterrupted {
		return nil
	} // Lost acknowledgement must not send a second SIGINT.
	if user {
		t.userInterrupted = true
	}
	// A worker the main agent is waiting for stops first; the main agent then
	// sees its delegate call end instead of hanging on it.
	if r.srv != nil {
		r.srv.interruptDelegation(r.sessID, t.thread)
	}
	// Keep slot ownership until the signal is sent. Otherwise a late interrupt
	// could target a newer turn's PID after this turn releases the thread.
	stop := t.stop
	if stop != nil {
		stop()
		return nil
	}
	sess, ok := r.srv.store.Get(r.sessID)
	if !ok || sess.ContainerID == "" {
		return nil
	}
	cmd, err := agent.InterruptCommandFor(agent.ChatPidFile(t.thread))
	if err != nil {
		return err
	}
	if err := r.srv.dock.ExecFireAndForget(ctx, sess.ContainerID, cmd); err != nil {
		log.Printf("interrupt %s: %v", r.sessID, err)
		return err
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func (m *chatManager) interruptAll(ctx context.Context) {
	type activeTurn struct {
		turn *chatTurn
		done <-chan struct{}
	}
	m.mu.Lock()
	var turns []activeTurn
	for _, room := range m.rooms {
		room.mu.Lock()
		for _, t := range room.runningLocked() {
			turns = append(turns, activeTurn{t, t.turnDone})
		}
		room.mu.Unlock()
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, turn := range turns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			turn.turn.interruptContext(ctx)
			select {
			case <-turn.done:
			case <-ctx.Done():
			}
		}()
	}
	wg.Wait()
}
