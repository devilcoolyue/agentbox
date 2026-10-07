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
	"agentbox/internal/store"
)

// titleTimeout bounds the one-shot model call that names a new thread.
const titleTimeout = 90 * time.Second

// --- chat manager: one room per session, broadcasting to all open tabs ---

type chatManager struct {
	srv   *Server
	mu    sync.Mutex
	rooms map[string]*chatRoom
}

func newChatManager(s *Server) *chatManager {
	return &chatManager{srv: s, rooms: map[string]*chatRoom{}}
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

	mu              sync.Mutex
	conns           map[*connWriter]bool
	running         bool
	turnDone        chan struct{}
	stop            func() // 当前回合的优雅中断（app-server 回合设置）；nil 时走 SIGINT
	requestID       string
	userInterrupted bool
	submitMu        sync.Mutex // receipt admission, orphan recovery and explicit review

	// fileMu guards the session's on-disk chat state: thread transcripts,
	// the active-thread pointer and the one-time legacy migration.
	fileMu sync.Mutex
}

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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return "running"
	}
	return "idle"
}

// tryBegin marks the room busy; returns false if a turn is already running.
func (r *chatRoom) tryBegin() bool {
	return r.begin() == nil
}

var errChatBusy = errors.New("chat room busy")

func (r *chatRoom) begin() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return errChatBusy
	}
	if r.srv != nil && r.srv.store != nil {
		if _, err := r.srv.store.PendingChatRequest(r.sessID); err == nil {
			return store.ErrChatPending
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	r.running = true
	r.requestID, r.userInterrupted = "", false
	r.turnDone = make(chan struct{})
	return nil
}

func (r *chatRoom) end() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
	r.requestID = ""
	if r.turnDone != nil {
		close(r.turnDone)
		r.turnDone = nil
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
func (r *chatRoom) recordChat(e logEntry, messageType string, retryText string) error {
	e.TS = time.Now()
	err := r.appendLog(e)
	r.broadcast(struct {
		logEntry
		Type      string `json:"type"`
		RetryText string `json:"retry_text,omitempty"`
	}{e, messageType, retryText})
	return err
}

func (r *chatRoom) appendLog(e logEntry) error {
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
	tid, err := r.srv.ensureActiveThread(sess)
	if err != nil {
		log.Printf("chat threads %s: %v", r.sessID, err)
		return err
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

	_ = cw.send(map[string]any{"type": "status", "state": room.state()})

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
			if err := room.begin(); err != nil {
				reject(chatRequestErrorCode(err))
				continue
			}
			if !s.spawn(func() { room.runTurnContext(turnContext, msg.Text, msg.Model, msg.Effort, msg.EffortControl) }) {
				room.end()
				reject("server_stopping")
				return
			}
		case "interrupt":
			room.interrupt()
		}
	}
}

// runTurn executes one headless agent turn: start container if needed, feed
// the prompt on stdin, stream JSONL events to every attached client, record
// the provider session id for resume.
func (r *chatRoom) runTurn(text, model, effort string, controls ...string) {
	r.runTurnContext(operationContext(r.srv.workContext()), text, model, effort, controls...)
}

func (r *chatRoom) runTurnContext(parent context.Context, text, model, effort string, controls ...string) {
	r.runTurnObserved(parent, text, model, effort, nil, controls...)
}

func (r *chatRoom) runTurnObserved(parent context.Context, text, model, effort string, receipt *chatReceiptTurn, controls ...string) {
	defer r.end()
	var observer chat.Observation
	if receipt != nil {
		observer = receipt
	}
	control := ""
	if len(controls) > 0 {
		control = controls[0]
	}
	service := chat.Service{Runtime: chatRuntime{r}, Usage: r.srv.usageService()}
	service.Run(parent, chat.Input{SessionID: r.sessID, Text: text, Model: model, Effort: effort, Control: control}, observer)
}

func (r *chatRoom) setStop(f func()) {
	r.mu.Lock()
	r.stop = f
	r.mu.Unlock()
}

// preTurnTitleState resolves the active thread and reports whether this turn is
// its first user message with no title yet — the trigger for title generation.
func (r *chatRoom) preTurnTitleState(sess store.Session) (tid string, firstTurn bool) {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	if err := r.srv.migrateThreads(sess); err != nil {
		return "", false
	}
	tid, err := r.srv.ensureActiveThread(sess)
	if err != nil {
		return "", false
	}
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
	if _, err := s.sessionAccount(sess); err != nil {
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
		titleTally.Observe(store.UsageEvent{
			User: sess.User, SessionID: sess.ID, ThreadID: tid, TurnID: store.NewID(),
			Agent: sess.Agent, AccountID: sess.AccountID, Kind: store.UsageKindTitle,
		}, usage)
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

func (r *chatRoom) interrupt() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = r.interruptRequest(ctx, "", true)
}

func (r *chatRoom) interruptContext(ctx context.Context) {
	_ = r.interruptRequest(ctx, "", false)
}

func (r *chatRoom) interruptRequest(ctx context.Context, id string, user bool) error {
	// app-server 回合注册了协议内的优雅中断，优先用它
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running || id != "" && r.requestID != id {
		return store.ErrChatConflict
	}
	if user && r.userInterrupted {
		return nil
	} // Lost acknowledgement must not send a second SIGINT.
	if user {
		r.userInterrupted = true
	}
	// Keep room ownership until the signal is sent. Otherwise a late interrupt
	// could target a newer turn's PID after this turn releases the room.
	stop := r.stop
	if stop != nil {
		stop()
		return nil
	}
	sess, ok := r.srv.store.Get(r.sessID)
	if !ok || sess.ContainerID == "" {
		return nil
	}
	if err := r.srv.dock.ExecFireAndForget(ctx, sess.ContainerID, agent.InterruptCommand()); err != nil {
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
		room *chatRoom
		done <-chan struct{}
	}
	m.mu.Lock()
	var turns []activeTurn
	for _, room := range m.rooms {
		room.mu.Lock()
		if room.running {
			turns = append(turns, activeTurn{room, room.turnDone})
		}
		room.mu.Unlock()
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, turn := range turns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			turn.room.interruptContext(ctx)
			select {
			case <-turn.done:
			case <-ctx.Done():
			}
		}()
	}
	wg.Wait()
}
