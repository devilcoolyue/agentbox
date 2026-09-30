package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

// turnTimeout bounds a single headless conversation turn. Coding-agent turns
// can legitimately run for many minutes while tests or builds execute.
const turnTimeout = 30 * time.Minute

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

	mu       sync.Mutex
	conns    map[*connWriter]bool
	running  bool
	turnDone chan struct{}
	stop     func() // 当前回合的优雅中断（app-server 回合设置）；nil 时走 SIGINT

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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	r.turnDone = make(chan struct{})
	return true
}

func (r *chatRoom) end() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
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
type chatTurnMetadata struct {
	ID           string `json:"id"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Control      string `json:"control"`
	Unsupported  bool   `json:"unsupported,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type logEntry struct {
	TS    time.Time         `json:"ts"`
	Kind  string            `json:"kind"` // "user" | "event" | "status" | "chat_session" | "title" | "divider"(旧)
	Text  string            `json:"text,omitempty"`
	Event json.RawMessage   `json:"event,omitempty"`
	State string            `json:"state,omitempty"`
	Error string            `json:"error,omitempty"`
	Turn  *chatTurnMetadata `json:"turn,omitempty"`
}

// History and live clients see the same timestamp and settings snapshot.
func (r *chatRoom) recordChat(e logEntry, messageType string, retryText string) {
	e.TS = time.Now()
	r.appendLog(e)
	r.broadcast(struct {
		logEntry
		Type      string `json:"type"`
		RetryText string `json:"retry_text,omitempty"`
	}{e, messageType, retryText})
}

func (r *chatRoom) appendLog(e logEntry) {
	sess, ok := r.srv.store.Get(r.sessID)
	if !ok {
		return
	}
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	if err := r.srv.migrateThreads(sess); err != nil {
		log.Printf("chat threads %s: %v", r.sessID, err)
		return
	}
	tid, err := r.srv.ensureActiveThread(sess)
	if err != nil {
		log.Printf("chat threads %s: %v", r.sessID, err)
		return
	}
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(r.srv.threadPath(sess, tid), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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
			return
		}
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		switch msg.Type {
		case "user_message":
			if msg.Text == "" {
				continue
			}
			// 额度拦在回合开始前：这里是唯一会花钱的入口，放进来就收不住了。
			if why := s.quotaBlock(sess.User); why != "" {
				_ = cw.send(map[string]any{"type": "error", "error": why})
				continue
			}
			if _, err := s.sessionAccount(sess); err != nil {
				_ = cw.send(map[string]any{"type": "error", "error": err.Error()})
				continue
			}
			if !room.tryBegin() {
				_ = cw.send(map[string]any{"type": "error", "error": "上一条消息仍在处理中，请等待或先中断"})
				continue
			}
			if !s.spawn(func() { room.runTurn(msg.Text, msg.Model, msg.Effort, msg.EffortControl) }) {
				room.end()
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
	defer r.end()
	s := r.srv

	// An in-flight turn may run for many minutes (tests/builds); hold the
	// session so the idle reaper never stops the container mid-turn.
	s.workspaces().Activity().Hold(r.sessID)
	defer s.workspaces().Activity().Release(r.sessID)

	fail := func(msg string) {
		r.recordChat(logEntry{Kind: "status", State: "error", Error: msg}, "status", "")
	}

	rejectOptions := func(message string) {
		r.recordChat(logEntry{Kind: "status", State: "error", Error: message}, "status", text)
	}

	ctx, cancel := context.WithTimeout(s.workContext(), turnTimeout)
	defer cancel()

	sess, ok := s.store.Get(r.sessID)
	if !ok {
		fail("session no longer exists")
		return
	}

	adapter, err := agent.Lookup(sess.Agent)
	if err != nil {
		fail(err.Error())
		return
	}
	if model == "" {
		model = sess.DefaultModel
	}

	control := ""
	if len(controls) > 0 {
		control = controls[0]
	}
	acct, err := s.sessionAccount(sess)
	if err != nil {
		fail(err.Error())
		return
	}
	options, err := agent.ResolveTurnOptions(sess.Agent, model, effort, control, s.cfg.ConfiguredReasoning(acct, model))
	if err != nil {
		rejectOptions(err.Error())
		return
	}

	// 记录发消息前该线程的状态：首条消息且尚无标题时，回合成功后据首条
	// 消息让模型生成一个简洁标题。房间占用期间线程不会被切换，tid 稳定，
	// 用量流水也挂在它上面。
	tid, firstTurn := r.preTurnTitleState(sess)

	// 本回合的用量归属。turnID 把一个回合里按模型拆出的多行流水串起来；一个
	// 回合可能报好几次用量，tally 负责收敛成一份账（见 usage.go）。
	// onLine 由回合的读循环串行调用，普通变量即可。
	turnID := store.NewID()
	var tally usageTally
	// 回合计时的起点，首字延迟与墙钟总耗时共用一块表：容器就绪之后开表（别把
	// 拉容器的几秒算进去），第一个模型输出事件量首字（见 Adapter.Decode），回合
	// 收尾时量总耗时。两个数同源才可比——provider 自报的 duration_ms 不含 CLI
	// 自身启动的那两秒，单独拿它当总耗时会比首字还小。
	var turnStart time.Time
	var ttftMS int64

	r.broadcast(map[string]any{"type": "status", "state": "running"})
	sess, err = s.startSession(ctx, sess)
	if err != nil {
		fail("启动容器失败: " + err.Error())
		return
	}
	if sess.Agent == config.AgentClaude {
		if err := s.workspaces().UseRunning(ctx, sess.ID, func(cur store.Session) error { return s.syncMCP(ctx, cur) }); err != nil {
			fail("MCP 配置尚未应用：" + err.Error())
			return
		}
	}
	// Re-resolve after startup: credentials/profile may have changed, and only
	// a running container can supply catalog metadata. No inference is started.
	acct, err = s.sessionAccount(sess)
	if err != nil {
		fail(err.Error())
		return
	}
	capability := s.cfg.ConfiguredReasoning(acct, model)
	if capability == nil && effort != "" {
		discovered, _ := s.discoverReasoning(ctx, sess, acct)
		if value, ok := discovered[model]; ok {
			capability = &value
		}
	}
	options, err = agent.ResolveTurnOptions(sess.Agent, model, effort, control, capability)
	if err != nil {
		rejectOptions(err.Error())
		return
	}
	if err := agent.CheckReasoningInheritance(sess.Agent, s.homeDir(sess), s.workspaceDir(sess), acct.Env, options); err != nil {
		rejectOptions(err.Error())
		return
	}
	if sess.Agent == config.AgentClaude && effort != "" && options.Control == "effort" {
		helpCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		help, probeErr := s.dock.ExecCapture(helpCtx, sess.ContainerID, []string{"claude", "--help"}, nil, "")
		cancel()
		if probeErr != nil || !strings.Contains(help, "--effort") {
			rejectOptions("当前容器 Claude Code 未确认支持原生 --effort，请更新镜像后重试，或配置为兼容的思考预算模式")
			return
		}
	}

	r.recordChat(logEntry{Kind: "user", Text: text, Turn: &chatTurnMetadata{
		ID: turnID, Model: model, Effort: effort, Control: options.Control,
		Unsupported: options.Unsupported, BudgetTokens: options.BudgetTokens,
	}}, "user_message", "")
	turnStart = time.Now()
	tally = s.usageService().NewChatTally(sess)

	// 容器起来之后才可能产生消耗，之后无论回合正常收尾、报错还是被中断，已经
	// 报上来的用量都要落库——钱花了就得记。一行都没记到说明这个 agent/版本报
	// 用量的形状我们没认出来：消耗真实发生了却不会进报表，宁可吵一句。
	defer func() {
		if r.flushUsage(&tally, time.Since(turnStart)) == 0 {
			log.Printf("usage: 会话 %s 回合结束但未记到用量（agent=%s），该回合消耗不会进报表", r.sessID, sess.Agent)
		}
		r.publishTurnCost(tid, turnID)
	}()

	// 两条路径共用的行处理：JSON 事件落盘并广播（增量只广播），
	// 顺带提取 provider 会话 id 供续聊；非 JSON 行按原文透传。
	chatID := sess.ChatSession
	onLine := func(line []byte) {
		if len(line) == 0 {
			return
		}
		if event, valid := adapter.Decode(line); valid {
			ev := event.Raw
			// 掐表要在增量分支之前：最早的模型输出往往就是个增量事件，放到
			// 后面量到的是第一个完整事件，那已经是整段话说完了。
			if ttftMS == 0 && event.Output {
				ttftMS = time.Since(turnStart).Milliseconds()
			}
			// Claude message_delta carries final output counts; meter it before
			// partial events take the broadcast-only return below.
			tally.Observe(store.UsageEvent{
				User: sess.User, SessionID: sess.ID, ThreadID: tid, TurnID: turnID,
				Agent: sess.Agent, AccountID: sess.AccountID, Model: model,
				Kind: store.UsageKindChat, TTFTMs: ttftMS,
			}, line)
			if event.Partial {
				// 增量 delta 只广播不落盘：随后的完整事件会带全文再来一份
				r.broadcast(map[string]any{"type": "agent_event", "event": ev, "ts": time.Now()})
				return
			}
			r.recordChat(logEntry{Kind: "event", Event: ev}, "agent_event", "")
			if id := event.SessionID; id != "" && id != chatID {
				chatID = id
				if _, err := s.store.Update(sess.ID, func(x *store.Session) { x.ChatSession = id }); err != nil {
					log.Printf("save chat session id %s: %v", sess.ID, err)
				}
				// 同时落进线程文件：切走再切回来时据此恢复上下文续聊
				r.appendLog(logEntry{Kind: "chat_session", Text: id})
			}
		} else {
			r.broadcast(map[string]any{"type": "agent_raw", "text": string(line)})
		}
	}

	// codex 优先走 app-server 协议（真流式增量）；容器里的 codex 太旧等
	// 握手失败的情况回退传统 exec 路径（无增量，前端整段回放兜底）。
	handled := false
	if adapter.Capabilities().AppServer {
		fallback, err := r.appServerTurn(ctx, sess, text, model, effort, onLine, options.Unsupported)
		switch {
		case err == nil:
			handled = true
		case fallback:
			log.Printf("codex app-server 不可用，回退 exec (%s): %v", r.sessID, err)
		default:
			fail(err.Error())
			return
		}
	}
	if !handled {
		if options.Unsupported {
			fail("当前 CLI 无法确认已省略默认或历史线程中的推理参数；请使用兼容该模型的 CLI / 中转配置，验证后再更新模型能力")
			return
		}
		cmd, err := adapter.Chat(s.cfg.GetPermissionMode(), chatID, model, effort, options.Control)
		if err != nil {
			fail(err.Error())
			return
		}
		if err := r.execTurn(ctx, sess, cmd, text, onLine); err != nil {
			fail(err.Error())
			return
		}
	}
	r.recordChat(logEntry{Kind: "status", State: "idle"}, "status", "")

	// 首条消息的对话：异步用模型总结出一个标题，不阻塞对话流。
	if firstTurn && tid != "" {
		s.spawn(func() { r.generateTitle(tid, text) })
	}
}

// execTurn 跑传统的一次性 CLI 回合：stdin 喂提示词后关闭，stdout 收 JSONL
// 事件直到进程退出。claude 一直走这里；codex 仅在 app-server 不可用时回退。
func (r *chatRoom) execTurn(ctx context.Context, sess store.Session, cmd []string, text string, onLine func([]byte)) error {
	s := r.srv
	env, err := s.execEnv(sess)
	if err != nil {
		return err
	}
	stream, err := s.dock.ExecStream(ctx, sess.ContainerID, cmd, env)
	if err != nil {
		return errors.New("exec失败: " + err.Error())
	}
	defer stream.Close()

	if _, err := stream.Write([]byte(text)); err != nil {
		return errors.New("写入提示词失败: " + err.Error())
	}
	if err := stream.CloseWrite(); err != nil {
		return errors.New("关闭stdin失败: " + err.Error())
	}

	pr, pw := io.Pipe()
	defer pr.Close() // 提前返回时解开 demux 的阻塞写
	stderr := &tailBuffer{max: 8 << 10}
	demuxDone := make(chan struct{})
	go func() {
		defer close(demuxDone)
		err := stream.Demux(pw, stderr)
		pw.CloseWithError(err)
	}()
	finishOutput := func() { stream.Close(); _ = pr.Close(); <-demuxDone }
	defer finishOutput()

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 1<<20), 32<<20) // single events can be large
	for scanner.Scan() {
		onLine(bytes.TrimSpace(scanner.Bytes()))
	}

	code, err := s.dock.ExitCode(ctx, stream.ExecID)
	if err != nil {
		return errors.New("等待退出码失败: " + err.Error())
	}
	if code != 0 {
		return errors.New("agent 进程退出码 " + itoa(code) + ": " + stderr.String())
	}
	return nil
}

// appServerTurn 经 codex app-server 协议跑一回合，事件由协议驱动器翻译后
// 进 onLine（含打字机用的流式增量）。返回 fallback=true 表示回合尚未开始
// （握手/开线程失败），可安全改走传统 exec 路径。
func (r *chatRoom) appServerTurn(ctx context.Context, sess store.Session, text, model, effort string, onLine func([]byte), unsupported ...bool) (fallback bool, err error) {
	s := r.srv
	env, err := s.execEnv(sess)
	if err != nil {
		return false, err
	}
	stream, err := s.dock.ExecStream(ctx, sess.ContainerID, agent.AppServerCommand(), env)
	if err != nil {
		return false, errors.New("exec失败: " + err.Error())
	}
	defer stream.Close()

	pr, pw := io.Pipe()
	defer pr.Close() // 提前返回时解开 demux 的阻塞写
	stderr := &tailBuffer{max: 8 << 10}
	demuxDone := make(chan struct{})
	go func() {
		defer close(demuxDone)
		err := stream.Demux(pw, stderr)
		pw.CloseWithError(err)
	}()
	finishOutput := func() { stream.Close(); _ = pr.Close(); <-demuxDone }
	defer finishOutput()

	// 中断优先走协议内的 turn/interrupt（回合优雅收尾、不惊动进程），
	// SIGINT 仅在协议没接管时兜底（见 interrupt）。
	ich := make(chan struct{})
	var once sync.Once
	r.setStop(func() { once.Do(func() { close(ich) }) })
	defer r.setStop(nil)

	err = agent.RunCodexTurn(ctx, stream, pr,
		func() { _ = stream.CloseWrite() }, // 硬断兜底：app-server 随 stdin EOF 退出
		ich,
		agent.CodexTurn{Prompt: text, ThreadID: sess.ChatSession, Model: model, Effort: effort, Cwd: dockerx.WorkspaceMount, RejectInheritedEffort: len(unsupported) > 0 && unsupported[0]},
		onLine)
	if err != nil {
		if errors.Is(err, agent.ErrAppServerUnavailable) {
			return true, err
		}
		finishOutput()
		if tail := stderr.String(); tail != "" {
			return false, fmt.Errorf("%v: %s", err, tail)
		}
		return false, err
	}

	_ = stream.CloseWrite() // 正常收尾：关 stdin 让 app-server 退出
	drainDone := make(chan struct{})
	go func() { defer close(drainDone); _, _ = io.Copy(io.Discard, pr) }()
	defer func() { finishOutput(); <-drainDone }()
	if code, err := s.dock.ExitCode(ctx, stream.ExecID); err != nil {
		log.Printf("codex app-server 等退出码 %s: %v", r.sessID, err)
	} else if code != 0 {
		finishOutput()
		// 回合已完整走完，退出码异常只记日志不打扰用户
		log.Printf("codex app-server 退出码 %d (%s): %s", code, r.sessID, stderr.String())
	}
	return false, nil
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
	r.interruptContext(ctx)
}

func (r *chatRoom) interruptContext(ctx context.Context) {
	// app-server 回合注册了协议内的优雅中断，优先用它
	r.mu.Lock()
	stop := r.stop
	r.mu.Unlock()
	if stop != nil {
		stop()
		return
	}
	sess, ok := r.srv.store.Get(r.sessID)
	if !ok || sess.ContainerID == "" {
		return
	}
	if err := r.srv.dock.ExecFireAndForget(ctx, sess.ContainerID, agent.InterruptCommand()); err != nil {
		log.Printf("interrupt %s: %v", r.sessID, err)
	}
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
