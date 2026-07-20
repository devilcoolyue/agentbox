package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"agentbox/internal/agent"
	"agentbox/internal/store"
)

// turnTimeout bounds a single headless conversation turn. Coding-agent turns
// can legitimately run for many minutes while tests or builds execute.
const turnTimeout = 30 * time.Minute

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

	mu      sync.Mutex
	conns   map[*connWriter]bool
	running bool
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	return true
}

func (r *chatRoom) end() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
}

// --- persistence: every chat event is appended to chat.jsonl so the UI can
// restore the transcript after the session (or server) restarts ---

type logEntry struct {
	TS    time.Time       `json:"ts"`
	Kind  string          `json:"kind"` // "user" | "event" | "status" | "divider"
	Text  string          `json:"text,omitempty"`
	Event json.RawMessage `json:"event,omitempty"`
	State string          `json:"state,omitempty"`
	Error string          `json:"error,omitempty"`
}

func (r *chatRoom) appendLog(e logEntry) {
	sess, ok := r.srv.store.Get(r.sessID)
	if !ok {
		return
	}
	e.TS = time.Now()
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(r.srv.chatLogPath(sess), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("chat log %s: %v", r.sessID, err)
		return
	}
	defer f.Close()
	f.Write(append(raw, '\n'))
}

// --- websocket endpoint ---

func (s *Server) handleChatWS(w http.ResponseWriter, r *http.Request, sess store.Session) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	room := s.chat.room(sess.ID)
	cw := &connWriter{c: conn}
	room.attach(cw)
	defer func() {
		room.detach(cw)
		conn.Close()
	}()

	_ = cw.send(map[string]any{"type": "status", "state": room.state()})

	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
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
			Type   string `json:"type"`
			Text   string `json:"text"`
			Model  string `json:"model"`
			Effort string `json:"effort"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		switch msg.Type {
		case "user_message":
			if msg.Text == "" {
				continue
			}
			if !room.tryBegin() {
				_ = cw.send(map[string]any{"type": "error", "error": "上一条消息仍在处理中，请等待或先中断"})
				continue
			}
			go room.runTurn(msg.Text, msg.Model, msg.Effort)
		case "interrupt":
			room.interrupt()
		}
	}
}

// runTurn executes one headless agent turn: start container if needed, feed
// the prompt on stdin, stream JSONL events to every attached client, record
// the provider session id for resume.
func (r *chatRoom) runTurn(text, model, effort string) {
	defer r.end()
	s := r.srv

	fail := func(msg string) {
		r.appendLog(logEntry{Kind: "status", State: "error", Error: msg})
		r.broadcast(map[string]any{"type": "status", "state": "error", "error": msg})
	}

	ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
	defer cancel()

	sess, ok := s.store.Get(r.sessID)
	if !ok {
		fail("session no longer exists")
		return
	}

	r.appendLog(logEntry{Kind: "user", Text: text})
	r.broadcast(map[string]any{"type": "user_message", "text": text})
	r.broadcast(map[string]any{"type": "status", "state": "running"})

	sess, err := s.startSession(ctx, sess)
	if err != nil {
		fail("启动容器失败: " + err.Error())
		return
	}

	cmd, err := agent.ChatCommand(sess.Agent, s.cfg.GetPermissionMode(), sess.ChatSession, model, effort)
	if err != nil {
		fail(err.Error())
		return
	}

	stream, err := s.dock.ExecStream(ctx, sess.ContainerID, cmd, s.execEnv(sess))
	if err != nil {
		fail("exec失败: " + err.Error())
		return
	}
	defer stream.Close()

	if _, err := stream.Write([]byte(text)); err != nil {
		fail("写入提示词失败: " + err.Error())
		return
	}
	if err := stream.CloseWrite(); err != nil {
		fail("关闭stdin失败: " + err.Error())
		return
	}

	pr, pw := io.Pipe()
	stderr := &tailBuffer{max: 8 << 10}
	go func() {
		err := stream.Demux(pw, stderr)
		pw.CloseWithError(err)
	}()

	chatID := sess.ChatSession
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 1<<20), 32<<20) // single events can be large
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if line[0] == '{' && json.Valid(line) {
			ev := make(json.RawMessage, len(line))
			copy(ev, line)
			if agent.IsPartialEvent(line) {
				// 增量 delta 只广播不落盘：随后的完整 assistant 事件会带全文再来一份
				r.broadcast(map[string]any{"type": "agent_event", "event": ev})
				continue
			}
			r.appendLog(logEntry{Kind: "event", Event: ev})
			r.broadcast(map[string]any{"type": "agent_event", "event": ev})
			if id := agent.ExtractSessionID(line); id != "" && id != chatID {
				chatID = id
				if _, err := s.store.Update(sess.ID, func(x *store.Session) { x.ChatSession = id }); err != nil {
					log.Printf("save chat session id %s: %v", sess.ID, err)
				}
			}
		} else {
			r.broadcast(map[string]any{"type": "agent_raw", "text": string(line)})
		}
	}

	code, err := s.dock.ExitCode(ctx, stream.ExecID)
	if err != nil {
		fail("等待退出码失败: " + err.Error())
		return
	}
	if code != 0 {
		fail("agent 进程退出码 " + itoa(code) + ": " + stderr.String())
		return
	}
	r.appendLog(logEntry{Kind: "status", State: "idle"})
	r.broadcast(map[string]any{"type": "status", "state": "idle"})
}

func (r *chatRoom) interrupt() {
	sess, ok := r.srv.store.Get(r.sessID)
	if !ok || sess.ContainerID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.srv.dock.ExecFireAndForget(ctx, sess.ContainerID, agent.InterruptCommand()); err != nil {
		log.Printf("interrupt %s: %v", r.sessID, err)
	}
}

// handleChatReset starts a fresh conversation thread: the provider-side
// session id is cleared so the next turn runs without resume, and a divider
// entry is recorded so the transcript shows where context was cut. Occupying
// the room prevents a turn from racing in and re-saving the old id.
func (s *Server) handleChatReset(w http.ResponseWriter, r *http.Request, sess store.Session) {
	room := s.chat.room(sess.ID)
	if !room.tryBegin() {
		writeErr(w, http.StatusConflict, "有消息正在处理中，请等待完成或先中断")
		return
	}
	defer room.end()
	cur, ok := s.store.Get(sess.ID)
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if cur.ChatSession == "" {
		writeJSON(w, http.StatusOK, map[string]any{"reset": false})
		return
	}
	if _, err := s.store.Update(sess.ID, func(x *store.Session) { x.ChatSession = "" }); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	room.appendLog(logEntry{Kind: "divider"})
	room.broadcast(map[string]any{"type": "divider"})
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

// handleHistory returns the persisted transcript. The log is split into
// conversation segments at divider entries ("新对话" resets); the default
// response carries only the latest segment in full plus a metadata list of
// earlier ones (archives) that the UI shows as collapsed rows. ?seg=N fetches
// one earlier segment's entries on demand.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request, sess store.Session) {
	const maxHistory = 2000
	raw, err := os.ReadFile(s.chatLogPath(sess))
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"entries": []any{}, "archives": []any{}})
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	segs := splitConversations(raw)

	if q := r.URL.Query().Get("seg"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 || n >= len(segs) {
			writeErr(w, http.StatusBadRequest, "无效的对话段序号")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"entries": segEntries(segs[n], maxHistory)})
		return
	}

	last := len(segs) - 1
	archives := []histArchive{}
	for i := 0; i < last; i++ {
		if a, ok := summarizeSeg(i, segs[i]); ok {
			archives = append(archives, a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":  segEntries(segs[last], maxHistory),
		"archives": archives,
	})
}

type histEntry struct {
	raw  json.RawMessage
	meta struct {
		TS   time.Time `json:"ts"`
		Kind string    `json:"kind"`
		Text string    `json:"text"`
	}
}

// histArchive is the collapsed-row metadata of one earlier conversation.
type histArchive struct {
	Seg     int       `json:"seg"`     // index into splitConversations output
	TS      time.Time `json:"ts"`      // when the conversation started
	Turns   int       `json:"turns"`   // user messages in the segment
	Preview string    `json:"preview"` // first user message, truncated
}

// splitConversations cuts chat.jsonl at divider entries. Each divider leads
// the segment it starts, so the latest conversation still renders its
// "新对话 · 时间" line at the top.
func splitConversations(raw []byte) [][]histEntry {
	segs := [][]histEntry{}
	cur := []histEntry{}
	for _, l := range bytes.Split(raw, []byte{'\n'}) {
		l = bytes.TrimSpace(l)
		if len(l) == 0 || !json.Valid(l) {
			continue
		}
		e := histEntry{raw: json.RawMessage(l)}
		_ = json.Unmarshal(l, &e.meta)
		if e.meta.Kind == "divider" {
			segs = append(segs, cur)
			cur = []histEntry{e}
			continue
		}
		cur = append(cur, e)
	}
	return append(segs, cur)
}

func segEntries(seg []histEntry, max int) []json.RawMessage {
	if len(seg) > max {
		seg = seg[len(seg)-max:]
	}
	out := make([]json.RawMessage, len(seg))
	for i, e := range seg {
		out[i] = e.raw
	}
	return out
}

// summarizeSeg builds the collapsed-row metadata; segments with nothing to
// show (double reset, only status noise) are dropped from the list.
func summarizeSeg(idx int, seg []histEntry) (histArchive, bool) {
	a := histArchive{Seg: idx}
	visible := false
	for _, e := range seg {
		switch e.meta.Kind {
		case "user":
			a.Turns++
			visible = true
			if a.Preview == "" {
				a.Preview = truncRunes(e.meta.Text, 120)
			}
		case "event":
			visible = true
		}
	}
	if !visible {
		return a, false
	}
	a.TS = seg[0].meta.TS
	return a, true
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// tailBuffer keeps only the last max bytes written (stderr diagnostics).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
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
