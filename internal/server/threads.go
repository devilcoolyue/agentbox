package server

// Conversation threads. Each chat thread of a session is one JSONL transcript
// at <sessionDir>/chats/<threadID>.jsonl; chats/active names the thread new
// turns append to. Every thread records the provider conversation id it can
// resume from as "chat_session" entries, so switching back to an earlier
// thread continues with its original context. The legacy single chat.jsonl
// (segmented by divider entries) is migrated on first access: each segment
// becomes a thread, with the resume id recovered from the raw events already
// on disk. All on-disk access goes through the room's fileMu.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/store"
)

var threadIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)

func newThreadID(ts time.Time) string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return ts.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func (s *Server) chatsDir(sess store.Session) string {
	return filepath.Join(s.sessionDir(sess), "chats")
}

func (s *Server) threadPath(sess store.Session, tid string) string {
	return filepath.Join(s.chatsDir(sess), tid+".jsonl")
}

func (s *Server) activeThreadFile(sess store.Session) string {
	return filepath.Join(s.chatsDir(sess), "active")
}

// activeThread returns the thread id new turns append to ("" if none yet).
// Callers hold the room's fileMu.
func (s *Server) activeThread(sess store.Session) string {
	raw, err := os.ReadFile(s.activeThreadFile(sess))
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(raw))
	if !threadIDRe.MatchString(id) {
		return ""
	}
	return id
}

func (s *Server) setActiveThread(sess store.Session, tid string) error {
	if tid == "" {
		err := os.Remove(s.activeThreadFile(sess))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.WriteFile(s.activeThreadFile(sess), []byte(tid), 0o600)
}

// ensureActiveThread returns the active thread id, minting one if the session
// has none (first message ever, or right after the last thread was deleted).
func (s *Server) ensureActiveThread(sess store.Session) (string, error) {
	if id := s.activeThread(sess); id != "" {
		return id, nil
	}
	id := newThreadID(time.Now())
	if err := s.setActiveThread(sess, id); err != nil {
		return "", err
	}
	return id, nil
}

// --- legacy migration ---

type histEntry struct {
	raw  json.RawMessage
	meta struct {
		TS    time.Time       `json:"ts"`
		Kind  string          `json:"kind"`
		Text  string          `json:"text"`
		Event json.RawMessage `json:"event"`
	}
}

// splitConversations cuts the legacy chat.jsonl at divider entries; each
// divider leads the segment it starts.
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

// migrateThreads splits the legacy single-file transcript into per-thread
// files. Idempotent and cheap once done (one stat). Callers hold fileMu.
func (s *Server) migrateThreads(sess store.Session) error {
	dir := s.chatsDir(sess)
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	legacy := s.chatLogPath(sess)
	raw, err := os.ReadFile(legacy)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	lastID := ""
	for i, seg := range splitConversations(raw) {
		var buf bytes.Buffer
		resume, visible := "", false
		ts := time.Now()
		for _, e := range seg {
			switch e.meta.Kind {
			case "divider":
				continue // 线程模型下分割线不再有意义
			case "user":
				visible = true
			case "event":
				visible = true
				// 落盘的原始事件里带着 provider 会话 id，反挖出来
				// 旧对话才能续聊；取最后一个（Claude 每轮 resume 会换新 id）
				if id := agent.ExtractSessionID(e.meta.Event); id != "" {
					resume = id
				}
			}
			if buf.Len() == 0 && !e.meta.TS.IsZero() {
				ts = e.meta.TS
			}
			buf.Write(e.raw)
			buf.WriteByte('\n')
		}
		if !visible {
			continue // 纯噪音段（连点新对话、只有状态行）不留
		}
		if resume != "" {
			line, _ := json.Marshal(logEntry{TS: ts, Kind: "chat_session", Text: resume})
			buf.Write(line)
			buf.WriteByte('\n')
		}
		id := fmt.Sprintf("%s-m%02d", ts.UTC().Format("20060102-150405"), i)
		if err := os.WriteFile(s.threadPath(sess, id), buf.Bytes(), 0o600); err != nil {
			return err
		}
		lastID = id
	}
	if lastID != "" {
		if err := s.setActiveThread(sess, lastID); err != nil {
			return err
		}
	}
	return os.Rename(legacy, legacy+".bak")
}

// --- thread metadata ---

type threadMeta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"` // 模型总结的标题，没有则回退到首条用户消息（截断）
	TS        time.Time `json:"ts"`
	Updated   time.Time `json:"updated"`
	Turns     int       `json:"turns"`
	Resumable bool      `json:"resumable"` // 是否记录了可续聊的 provider 会话 id
}

// scanThread derives list metadata from one thread file. Callers hold fileMu.
func (s *Server) scanThread(sess store.Session, tid string) (threadMeta, bool) {
	raw, err := os.ReadFile(s.threadPath(sess, tid))
	if err != nil {
		return threadMeta{}, false
	}
	m := threadMeta{ID: tid}
	firstMsg, genTitle := "", ""
	for _, e := range parseEntries(raw) {
		if m.TS.IsZero() && !e.meta.TS.IsZero() {
			m.TS = e.meta.TS
		}
		if !e.meta.TS.IsZero() {
			m.Updated = e.meta.TS
		}
		switch e.meta.Kind {
		case "user":
			m.Turns++
			if firstMsg == "" {
				firstMsg = e.meta.Text
			}
		case "title":
			if e.meta.Text != "" {
				genTitle = e.meta.Text
			}
		case "chat_session":
			m.Resumable = e.meta.Text != ""
		}
	}
	// 优先用模型总结的标题，没有则回退到首条用户消息（截断）
	if genTitle != "" {
		m.Title = truncRunes(genTitle, 120)
	} else {
		m.Title = truncRunes(firstMsg, 120)
	}
	return m, true
}

// threadTitleSource returns the thread's first user message, its user-turn
// count, and whether a model-generated title has already been stored. Callers
// hold fileMu.
func (s *Server) threadTitleSource(sess store.Session, tid string) (firstMsg string, userTurns int, hasTitle bool) {
	raw, err := os.ReadFile(s.threadPath(sess, tid))
	if err != nil {
		return "", 0, false
	}
	for _, e := range parseEntries(raw) {
		switch e.meta.Kind {
		case "user":
			userTurns++
			if firstMsg == "" {
				firstMsg = e.meta.Text
			}
		case "title":
			if e.meta.Text != "" {
				hasTitle = true
			}
		}
	}
	return firstMsg, userTurns, hasTitle
}

// appendThreadEntry appends one entry to a specific thread file (not
// necessarily the active one). Callers hold fileMu.
func (s *Server) appendThreadEntry(sess store.Session, tid string, e logEntry) error {
	e.TS = time.Now()
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.threadPath(sess, tid), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

func parseEntries(raw []byte) []histEntry {
	out := []histEntry{}
	for _, l := range bytes.Split(raw, []byte{'\n'}) {
		l = bytes.TrimSpace(l)
		if len(l) == 0 || !json.Valid(l) {
			continue
		}
		e := histEntry{raw: json.RawMessage(l)}
		_ = json.Unmarshal(l, &e.meta)
		out = append(out, e)
	}
	return out
}

// threadResumeID is the provider conversation id a thread continues from:
// the last chat_session entry ("" when the thread predates id recording).
func (s *Server) threadResumeID(sess store.Session, tid string) string {
	raw, err := os.ReadFile(s.threadPath(sess, tid))
	if err != nil {
		return ""
	}
	resume := ""
	for _, e := range parseEntries(raw) {
		if e.meta.Kind == "chat_session" && e.meta.Text != "" {
			resume = e.meta.Text
		}
	}
	return resume
}

func (s *Server) listThreads(sess store.Session) []threadMeta {
	out := []threadMeta{}
	entries, err := os.ReadDir(s.chatsDir(sess))
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		tid := strings.TrimSuffix(name, ".jsonl")
		if !threadIDRe.MatchString(tid) {
			continue
		}
		if m, ok := s.scanThread(sess, tid); ok && (m.Turns > 0 || !m.Updated.IsZero()) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// --- HTTP handlers ---

// historyLimits bounds one page of GET history. Pages end at a user message so
// each holds whole turns; only a turn larger than the hard limit is split, and
// the page starting inside it carries a turn_context line instead.
type historyLimits struct {
	Entries, Bytes             int // stop at the next user message once reached
	MaxEntries, MaxBytes       int // split a single turn beyond this
	ReloadEntries, ReloadBytes int // a from= refresh larger than this returns the newest page
}

// Counted in rendered entries: Claude's tool results are neither sent nor
// counted, they made up a third of a tool-heavy transcript.
var historyPage = historyLimits{
	Entries: 800, Bytes: 4 << 20,
	MaxEntries: 3000, MaxBytes: 16 << 20,
	ReloadEntries: 12000, ReloadBytes: 64 << 20,
}

// historySent reports whether the web client renders anything from an entry.
// Claude tool results ("user" events) only feed the CLI; they are often whole
// file contents, so history leaves them on disk.
func historySent(e histEntry) bool {
	if e.meta.Kind != "event" {
		return true
	}
	var ev struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(e.meta.Event, &ev) != nil || ev.Type != "user"
}

// historyStart picks the first index of the page ending before end.
func historyStart(entries []histEntry, end int, limits historyLimits) int {
	count, size, start := 0, 0, end
	for start > 0 {
		e := entries[start-1]
		if historySent(e) {
			count++
			size += len(e.raw)
		}
		start--
		if count >= limits.MaxEntries || size >= limits.MaxBytes {
			break
		}
		if e.meta.Kind == "user" && (count >= limits.Entries || size >= limits.Bytes) {
			break
		}
	}
	return start
}

func historyWithin(entries []histEntry, count, size int) bool {
	n, total := 0, 0
	for _, e := range entries {
		if historySent(e) {
			n++
			total += len(e.raw)
		}
		if n > count || total > size {
			return false
		}
	}
	return true
}

// historyIndex parses a page cursor. Cursors index the valid JSONL lines of a
// thread file, which is only ever appended to.
func historyIndex(value string, lo, hi int) (int, bool) {
	n, err := strconv.Atoi(value)
	return n, err == nil && n >= lo && n <= hi
}

// handleHistory returns the active thread's newest page plus its metadata for
// the thread bar. ?thread=&before= returns the page ending at that cursor of
// the named thread; ?thread=&from= re-reads the active thread from a cursor so
// a reconnect keeps pages the client already loaded. Responses carry start
// (cursor of the first returned entry) and has_more.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request, sess store.Session) {
	q := r.URL.Query()
	room := s.chat.room(sess.ID)
	room.fileMu.Lock()
	defer room.fileMu.Unlock()
	if err := s.migrateThreads(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	older := q.Has("before")
	tid := s.activeThread(sess)
	if older {
		tid = q.Get("thread")
		if !threadIDRe.MatchString(tid) {
			writeProblem(w, r, "chat.history", "invalid_request")
			return
		}
	}
	// New composers request a stable identity even for a not-yet-sent thread.
	// The legacy response/empty-thread behavior stays available to old clients.
	if !older && q.Get("draft_context") == "1" {
		var err error
		tid, err = s.ensureActiveThread(sess)
		if err != nil {
			writeProblem(w, r, "chat.history", "internal_error")
			return
		}
		if _, err = os.Stat(s.threadPath(sess, tid)); os.IsNotExist(err) {
			// A metadata-only entry makes an unsent draft's thread navigable after
			// switching away. It contains no prompt and is ignored by old renderers.
			err = s.appendThreadEntry(sess, tid, logEntry{Kind: "draft_context"})
		}
		if err != nil {
			writeProblem(w, r, "chat.history", "internal_error")
			return
		}
	}
	if tid == "" {
		writeJSON(w, http.StatusOK, map[string]any{"entries": []any{}, "thread": nil, "start": 0, "has_more": false})
		return
	}
	raw, err := os.ReadFile(s.threadPath(sess, tid))
	if err != nil {
		if os.IsNotExist(err) && older {
			writeProblem(w, r, "chat.history", "invalid_request")
			return
		}
		if os.IsNotExist(err) { // 新开的空对话还没落盘
			writeJSON(w, http.StatusOK, map[string]any{"entries": []any{}, "thread": nil, "active_thread": tid, "start": 0, "has_more": false})
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	entries := parseEntries(raw)
	end, start := len(entries), -1
	if older {
		var ok bool
		if end, ok = historyIndex(q.Get("before"), 1, len(entries)); !ok {
			writeProblem(w, r, "chat.history", "invalid_request")
			return
		}
	} else if q.Get("thread") == tid && q.Has("from") {
		// A stale or oversized refresh quietly falls back to the newest page;
		// the client compares start with what it asked for.
		if from, ok := historyIndex(q.Get("from"), 0, len(entries)); ok &&
			historyWithin(entries[from:], historyPage.ReloadEntries, historyPage.ReloadBytes) {
			start = from
		}
	}
	if start < 0 {
		start = historyStart(entries, end, historyPage)
	}
	var contextLine json.RawMessage
	// A page can begin inside a long turn. Preserve its request snapshot
	// without returning the potentially large original prompt.
	if start > 0 && start < end && entries[start].meta.Kind != "user" {
		for i := start - 1; i >= 0; i-- {
			if entries[i].meta.Kind == "user" {
				var user logEntry
				if json.Unmarshal(entries[i].raw, &user) == nil && user.Turn != nil {
					contextLine, _ = json.Marshal(logEntry{TS: user.TS, Kind: "turn_context", Turn: user.Turn})
				}
				break
			}
		}
	}
	lines := make([]json.RawMessage, 0, end-start+1)
	if contextLine != nil {
		lines = append(lines, contextLine)
	}
	for _, e := range entries[start:end] {
		if historySent(e) {
			lines = append(lines, e.raw)
		}
	}
	var turnIDs []string
	for _, line := range lines {
		var entry struct {
			Turn *chatTurnMetadata `json:"turn"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Turn != nil {
			turnIDs = append(turnIDs, entry.Turn.ID)
		}
	}
	costs, err := s.store.ChatTurnCosts(sess.ID, tid, turnIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"entries": lines, "costs": costs, "start": start, "has_more": start > 0}
	if older {
		out["thread_id"] = tid
	} else {
		meta, _ := s.scanThread(sess, tid)
		out["thread"], out["active_thread"] = meta, tid
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleThreadList(w http.ResponseWriter, r *http.Request, sess store.Session) {
	room := s.chat.room(sess.ID)
	room.fileMu.Lock()
	defer room.fileMu.Unlock()
	if err := s.migrateThreads(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"threads": s.listThreads(sess),
		"active":  s.activeThread(sess),
	})
}

// handleThreadNew starts a fresh conversation thread and makes it active.
// The previous thread keeps its transcript and resume id, so it can be
// switched back to at any time. Occupying the room prevents a turn from
// racing in and appending to the wrong thread.
func (s *Server) handleThreadNew(w http.ResponseWriter, r *http.Request, sess store.Session) {
	room := s.chat.room(sess.ID)
	if err := room.begin(); err != nil {
		writeChatRequestError(w, r, err)
		return
	}
	defer room.end()
	room.fileMu.Lock()
	defer room.fileMu.Unlock()
	if err := s.migrateThreads(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 当前已是没发过消息的空对话：直接复用，不堆积空线程
	if tid := s.activeThread(sess); tid != "" {
		if _, err := os.Stat(s.threadPath(sess, tid)); os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"id": tid, "created": false})
			return
		}
		if raw, err := os.ReadFile(s.threadPath(sess, tid)); err == nil {
			entries := parseEntries(raw)
			if len(entries) == 1 && entries[0].meta.Kind == "draft_context" {
				writeJSON(w, http.StatusOK, map[string]any{"id": tid, "created": false})
				return
			}
		}
	}
	tid := newThreadID(time.Now())
	if err := s.setActiveThread(sess, tid); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.store.Update(sess.ID, func(x *store.Session) { x.ChatSession = "" }); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	room.broadcast(map[string]any{"type": "thread", "id": tid})
	writeJSON(w, http.StatusOK, map[string]any{"id": tid, "created": true})
}

// handleThreadActivate switches the active thread and restores its provider
// conversation id, so the next turn resumes that thread's context.
func (s *Server) handleThreadActivate(w http.ResponseWriter, r *http.Request, sess store.Session) {
	tid := r.PathValue("tid")
	if !threadIDRe.MatchString(tid) {
		writeErr(w, http.StatusBadRequest, "无效的对话 id")
		return
	}
	room := s.chat.room(sess.ID)
	if err := room.begin(); err != nil {
		writeChatRequestError(w, r, err)
		return
	}
	defer room.end()
	room.fileMu.Lock()
	defer room.fileMu.Unlock()
	if err := s.migrateThreads(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := os.Stat(s.threadPath(sess, tid)); err != nil {
		writeErr(w, http.StatusNotFound, "对话不存在")
		return
	}
	resume := s.threadResumeID(sess, tid)
	if err := s.setActiveThread(sess, tid); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.store.Update(sess.ID, func(x *store.Session) { x.ChatSession = resume }); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	room.broadcast(map[string]any{"type": "thread", "id": tid})
	writeJSON(w, http.StatusOK, map[string]any{"id": tid, "resumable": resume != ""})
}

// handleThreadRename gives a thread a user-chosen title. Titles live as "title"
// entries in the transcript and the newest one wins, so a rename is just one
// more appended entry — no rewrite of the JSONL, and the model-generated title
// it replaces stays in history.
func (s *Server) handleThreadRename(w http.ResponseWriter, r *http.Request, sess store.Session) {
	tid := r.PathValue("tid")
	if !threadIDRe.MatchString(tid) {
		writeErr(w, http.StatusBadRequest, "无效的对话 id")
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	title := truncRunes(strings.TrimSpace(req.Title), 120)
	if title == "" {
		writeErr(w, http.StatusBadRequest, "标题不能为空")
		return
	}
	room := s.chat.room(sess.ID)
	room.fileMu.Lock()
	if err := s.migrateThreads(sess); err != nil {
		room.fileMu.Unlock()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := os.Stat(s.threadPath(sess, tid)); err != nil {
		room.fileMu.Unlock()
		writeErr(w, http.StatusNotFound, "对话不存在")
		return
	}
	err := s.appendThreadEntry(sess, tid, logEntry{Kind: "title", Text: title})
	room.fileMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 其它打开着同一会话的标签页同步改名
	room.broadcast(map[string]any{"type": "thread_title", "id": tid, "title": title})
	writeJSON(w, http.StatusOK, map[string]string{"id": tid, "title": title})
}

func (s *Server) handleThreadDelete(w http.ResponseWriter, r *http.Request, sess store.Session) {
	tid := r.PathValue("tid")
	if !threadIDRe.MatchString(tid) {
		writeErr(w, http.StatusBadRequest, "无效的对话 id")
		return
	}
	room := s.chat.room(sess.ID)
	// 统一占房：执行中删除当前对话会抽掉正在写的文件，一律等回合结束
	if err := room.begin(); err != nil {
		writeChatRequestError(w, r, err)
		return
	}
	defer room.end()
	room.fileMu.Lock()
	defer room.fileMu.Unlock()
	if err := s.migrateThreads(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if u, ok := s.store.GetUser(sess.User); ok {
		if err := s.store.DeleteChatThreadRequests(u, sess.ID, tid); err != nil {
			writeChatRequestError(w, r, err)
			return
		}
	}
	if err := os.Remove(s.threadPath(sess, tid)); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	active := s.activeThread(sess)
	if active == tid {
		// 当前对话被删：切到最近的一条，没有就回到空白新对话
		next, resume := "", ""
		if list := s.listThreads(sess); len(list) > 0 {
			next = list[0].ID
			resume = s.threadResumeID(sess, next)
		}
		if err := s.setActiveThread(sess, next); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if _, err := s.store.Update(sess.ID, func(x *store.Session) { x.ChatSession = resume }); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		room.broadcast(map[string]any{"type": "thread", "id": next})
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "active": next})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "active": active})
}
