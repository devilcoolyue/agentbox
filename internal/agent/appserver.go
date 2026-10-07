package agent

// codex app-server 协议驱动：codex exec --json 不输出文本增量（agent_message
// 只在 item.completed 一次性带全文），要真流式只能走 app-server 的 stdio
// JSON-RPC 协议（item/agentMessage/delta 等通知逐字到达）。本文件把一次
// 无头对话回合翻译成两套既有事件词汇，链路其余部分零改动：
//
//   - 增量 → Claude 风格 stream_event（content_block_start/delta/stop），
//     前端打字机管线原样消费，服务端只广播不落盘（IsPartialEvent 识别）；
//   - 完整事件 → exec --json 的 item.* / turn.* 形状，落盘格式与旧对话
//     一致，历史渲染与 thread/回合 id 提取（ExtractSessionID）都不用改。
//
// 会话存储与 exec 同一套 rollout 文件（~/.codex/sessions），exec 记下的
// thread id 可被 thread/resume 无缝续上，反之亦然——新旧路径可互相回退。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
)

// ErrAppServerUnavailable 标记回合尚未提交前的失败（握手/开线程阶段）。
// 此时模型没跑任何东西，调用方可安全回退到传统 codex exec 路径重试。
var ErrAppServerUnavailable = errors.New("codex app-server 不可用")

// handshakeTimeout 兜住 initialize→turn/start 阶段（含大 rollout 的
// thread/resume）；超时按不可用处理，回退 exec。
const handshakeTimeout = 120 * time.Second

// interruptGrace 是发出 turn/interrupt 后等待回合优雅收尾的时长，
// 超过就硬断连接（关 stdin，app-server 随之退出）。
const interruptGrace = 10 * time.Second

// AppServerCommand 返回启动 codex app-server 的 argv。与 ChatCommand 相同的
// sh 包装记录 PID，保住既有的 SIGINT 兜底中断路径（exec 后 PID 不变）。
func AppServerCommand() []string {
	return []string{"/bin/sh", "-c", "echo $$ >" + PidFile + "; exec codex app-server"}
}

// CodexTurn 描述经 app-server 跑的一个无头对话回合。
type CodexTurn struct {
	Prompt                string
	ThreadID              string // 要续聊的 provider thread id；空则新开线程
	Model                 string // 可选的每回合模型覆盖
	Effort                string // 可选的推理强度（minimal|low|medium|high|xhigh）
	Cwd                   string // agent 的工作根目录（容器内路径）
	RejectInheritedEffort bool   // unsupported model: refuse retained thread settings
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type appServerConn struct {
	wmu  sync.Mutex // interrupt 协程与主流程并发写 stdin
	w    io.Writer
	sc   *bufio.Scanner
	emit func(line []byte)

	block    string         // 当前流式块："" | "text" | "thinking"
	sawDelta bool           // 当前 thinking 块内是否已有内容（summary 分段用分隔符续接）
	usage    map[string]any // 最近一次 tokenUsage，turn.completed 时带出

	mu     sync.Mutex // 保护 turnID（interrupt 协程读）
	thread string
	turn   string
}

// RunCodexTurn 在已建立的 app-server 进程流上驱动一个回合：握手、开/续线程、
// 提交用户消息，把通知翻译成 emit 的 JSONL 事件，直到回合结束。interrupt
// 触发优雅中断（turn/interrupt），abort 硬断连接（如关闭 stdin）用于兜底。
// 返回 ErrAppServerUnavailable（可回退 exec）或其它不可回退错误。
func RunCodexTurn(ctx context.Context, w io.Writer, r io.Reader, abort func(), interrupt <-chan struct{}, t CodexTurn, emit func(line []byte)) error {
	if t.Model != "" && !modelRe.MatchString(t.Model) {
		return fmt.Errorf("模型名 %q 无效", t.Model)
	}
	if t.Effort != "" && !codexEfforts[t.Effort] {
		return fmt.Errorf("思考强度 %q 无效", t.Effort)
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 32<<20) // aggregatedOutput 等单事件可能很大
	c := &appServerConn{w: w, sc: sc, emit: emit}

	// 看门狗：ctx 取消 / 用户中断 / 握手卡死时把连接断掉，解开阻塞的读循环
	done := make(chan struct{})
	defer close(done)
	turnStarted := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			abort()
		case <-time.After(handshakeTimeout):
			select {
			case <-turnStarted: // 回合已在跑，长回合是常态，不动
				select {
				case <-done:
				case <-ctx.Done():
					abort()
				}
			default:
				abort()
			}
		}
	}()
	go func() {
		select {
		case <-done:
			return
		case <-interrupt:
		}
		c.mu.Lock()
		threadID, turnID := c.thread, c.turn
		c.mu.Unlock()
		if turnID == "" { // 回合都没开成，直接断
			abort()
			return
		}
		c.send(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "turn/interrupt",
			"params": map[string]any{"threadId": threadID, "turnId": turnID}})
		select {
		case <-done:
		case <-time.After(interruptGrace):
			abort()
		}
	}()

	// --- 握手与线程 ---
	if _, err := c.call(1, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "agentbox", "version": "1.0"},
	}); err != nil {
		return fmt.Errorf("%w: initialize: %v", ErrAppServerUnavailable, err)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "initialized"})

	var threadRes json.RawMessage
	if t.ThreadID != "" {
		params := map[string]any{"threadId": t.ThreadID}
		if t.Model != "" {
			params["model"] = t.Model
		}
		res, err := c.call(2, "thread/resume", params)
		if err == nil {
			threadRes = res
		}
		// 续不上（rollout 丢失等）就静默新开线程，对话还能继续只是丢上下文
	}
	if threadRes == nil {
		params := map[string]any{"cwd": t.Cwd, "sandbox": "danger-full-access", "approvalPolicy": "never"}
		if t.Model != "" {
			params["model"] = t.Model
		}
		res, err := c.call(3, "thread/start", params)
		if err != nil {
			return fmt.Errorf("%w: thread/start: %v", ErrAppServerUnavailable, err)
		}
		threadRes = res
	}
	var tr struct {
		ReasoningEffort string `json:"reasoningEffort"`
		Thread          struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(threadRes, &tr); tr.Thread.ID == "" {
		return fmt.Errorf("%w: 线程响应缺 id", ErrAppServerUnavailable)
	}
	if t.RejectInheritedEffort && tr.ReasoningEffort != "" {
		return fmt.Errorf("模型 %s 不支持调整推理强度，但 CLI / 线程仍使用 %s；请清除 CLI 配置或新建对话后重试", t.Model, tr.ReasoningEffort)
	}
	c.mu.Lock()
	c.thread = tr.Thread.ID
	c.mu.Unlock()
	// 与 exec --json 同形状的 thread.started：落盘后 ExtractSessionID 取 id 续聊
	c.emitJSON(map[string]any{"type": "thread.started", "thread_id": tr.Thread.ID})

	// --- 提交回合 ---
	// 容器本身就是沙箱，与 exec 路径的 --dangerously-bypass 语义对齐
	turnParams := map[string]any{
		"threadId":       tr.Thread.ID,
		"input":          []map[string]any{{"type": "text", "text": t.Prompt}},
		"cwd":            t.Cwd,
		"sandboxPolicy":  map[string]any{"type": "dangerFullAccess"},
		"approvalPolicy": "never",
	}
	if t.Model != "" {
		turnParams["model"] = t.Model
	}
	if t.Effort != "" {
		turnParams["effort"] = t.Effort
	}
	// 内置模型默认不出推理摘要，reasoning 项的 summary 为空，网页「思考过程」
	// 无字可显，只能显式要。标记为不支持推理的模型不加任何推理相关覆盖。
	if !t.RejectInheritedEffort {
		turnParams["summary"] = CodexReasoningSummary
	}
	turnRes, err := c.call(4, "turn/start", turnParams)
	if err != nil {
		return fmt.Errorf("turn/start: %w", err)
	}
	var ur struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	json.Unmarshal(turnRes, &ur)
	c.mu.Lock()
	c.turn = ur.Turn.ID
	c.mu.Unlock()
	close(turnStarted)

	// --- 事件循环，直到 turn/completed ---
	for {
		m, err := c.next()
		if err != nil {
			return fmt.Errorf("app-server 连接中断: %w", err)
		}
		if m.Method == "" {
			continue // 响应（turn/interrupt 等），无需处理
		}
		if m.ID != nil { // server→client 请求；approvalPolicy=never 下不该出现，回绝以免卡死
			c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID,
				"error": map[string]any{"code": -32601, "message": "unsupported"}})
			continue
		}
		if finished := c.handleNotification(m.Method, m.Params); finished {
			return nil
		}
	}
}

// call 发出请求并等待对应响应，途中到达的通知照常翻译分发。
func (c *appServerConn) call(id int, method string, params map[string]any) (json.RawMessage, error) {
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	want := strconv.Itoa(id)
	for {
		m, err := c.next()
		if err != nil {
			return nil, err
		}
		if m.Method != "" {
			if m.ID != nil { // server→client 请求：回绝，别让对端干等
				c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID,
					"error": map[string]any{"code": -32601, "message": "unsupported"}})
			} else {
				c.handleNotification(m.Method, m.Params)
			}
			continue
		}
		if string(m.ID) != want {
			continue
		}
		if m.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		return m.Result, nil
	}
}

func (c *appServerConn) send(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(raw, '\n'))
	return err
}

// next 读下一条 JSON-RPC 消息，跳过空行与非 JSON 噪声。
func (c *appServerConn) next() (*rpcMsg, error) {
	for c.sc.Scan() {
		line := c.sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var m rpcMsg
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		return &m, nil
	}
	if err := c.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (c *appServerConn) emitJSON(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.emit(raw)
}

// --- 通知 → 事件翻译 ---

type asItem struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Command  string `json:"command"`
	ExitCode *int   `json:"exitCode"`
	Status   string `json:"status"`
	Query    string `json:"query"`
	Summary  []any  `json:"summary"`
	Changes  []struct {
		Path string `json:"path"`
	} `json:"changes"`
}

// handleNotification 翻译一条服务端通知；回合结束返回 true。
func (c *appServerConn) handleNotification(method string, params json.RawMessage) bool {
	switch method {
	case "item/started", "item/completed":
		var p struct {
			Item asItem `json:"item"`
		}
		if json.Unmarshal(params, &p) != nil {
			return false
		}
		c.translateItem(method == "item/completed", p.Item)
	case "item/agentMessage/delta":
		c.openBlock("text")
		c.emitDelta("text_delta", "text", deltaOf(params))
	case "item/reasoning/summaryPartAdded":
		if c.block == "thinking" && c.sawDelta {
			c.emitDelta("thinking_delta", "thinking", "\n\n")
		}
	case "item/reasoning/summaryTextDelta":
		c.openBlock("thinking")
		c.sawDelta = true
		c.emitDelta("thinking_delta", "thinking", deltaOf(params))
	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage struct {
				Last struct {
					Input  int `json:"inputTokens"`
					Cached int `json:"cachedInputTokens"`
					Output int `json:"outputTokens"`
				} `json:"last"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(params, &p) == nil {
			c.usage = map[string]any{
				"input_tokens":        p.TokenUsage.Last.Input,
				"cached_input_tokens": p.TokenUsage.Last.Cached,
				"output_tokens":       p.TokenUsage.Last.Output,
			}
		}
	case "turn/completed":
		var p struct {
			Turn struct {
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		json.Unmarshal(params, &p)
		c.closeBlock()
		switch p.Turn.Status {
		case "completed":
			ev := map[string]any{"type": "turn.completed"}
			if c.usage != nil {
				ev["usage"] = c.usage
			}
			c.emitJSON(ev)
		case "interrupted":
			c.emitJSON(map[string]any{"type": "turn.failed", "status": "interrupted",
				"error": map[string]any{"message": "回合已中断"}})
		default:
			msg := "回合失败"
			if p.Turn.Error != nil && p.Turn.Error.Message != "" {
				msg = p.Turn.Error.Message
			}
			c.emitJSON(map[string]any{"type": "turn.failed",
				"error": map[string]any{"message": msg}})
		}
		return true
	case "error":
		var p struct {
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(params, &p)
		msg := p.Message
		if msg == "" {
			msg = p.Error.Message
		}
		if msg != "" {
			c.emitJSON(map[string]any{"type": "error", "message": msg})
		}
	}
	return false
}

// translateItem 把 v2 的 thread item 映射为 exec --json 的 item.* 事件形状。
func (c *appServerConn) translateItem(completed bool, it asItem) {
	switch it.Type {
	case "agentMessage":
		if !completed {
			c.openBlock("text") // 光标先亮起来，delta 随后就到
			return
		}
		c.closeBlock()
		if it.Text != "" {
			c.emitJSON(map[string]any{"type": "item.completed",
				"item": map[string]any{"type": "agent_message", "text": it.Text}})
		}
	case "reasoning":
		if !completed {
			c.openBlock("thinking")
			return
		}
		c.closeBlock()
		if text := joinSummary(it.Summary); text != "" {
			c.emitJSON(map[string]any{"type": "item.completed",
				"item": map[string]any{"type": "reasoning", "text": text}})
		}
	case "commandExecution":
		if !completed {
			c.emitJSON(map[string]any{"type": "item.started",
				"item": map[string]any{"type": "command_execution", "command": it.Command}})
			return
		}
		ev := map[string]any{"type": "command_execution", "command": it.Command, "status": it.Status}
		if it.ExitCode != nil {
			ev["exit_code"] = *it.ExitCode
		}
		c.emitJSON(map[string]any{"type": "item.completed", "item": ev})
	case "fileChange":
		if !completed {
			return
		}
		changes := make([]map[string]any, 0, len(it.Changes))
		for _, ch := range it.Changes {
			changes = append(changes, map[string]any{"path": ch.Path})
		}
		c.emitJSON(map[string]any{"type": "item.completed",
			"item": map[string]any{"type": "file_change", "changes": changes}})
	case "webSearch":
		if completed && it.Query != "" {
			c.emitJSON(map[string]any{"type": "item.completed",
				"item": map[string]any{"type": "web_search", "query": it.Query}})
		}
	}
}

// openBlock/closeBlock 维护流式块的 start/stop 括号，形状对齐 Claude 的
// stream_event，前端打字机据此建块、收尾。
func (c *appServerConn) openBlock(kind string) {
	if c.block == kind {
		return
	}
	c.closeBlock()
	c.block = kind
	c.sawDelta = false
	blockType := "text"
	if kind == "thinking" {
		blockType = "thinking"
	}
	c.emitJSON(map[string]any{"type": "stream_event", "event": map[string]any{
		"type": "content_block_start", "content_block": map[string]any{"type": blockType}}})
}

func (c *appServerConn) closeBlock() {
	if c.block == "" {
		return
	}
	c.block = ""
	c.emitJSON(map[string]any{"type": "stream_event",
		"event": map[string]any{"type": "content_block_stop"}})
}

func (c *appServerConn) emitDelta(deltaType, field, text string) {
	if text == "" {
		return
	}
	c.emitJSON(map[string]any{"type": "stream_event", "event": map[string]any{
		"type":  "content_block_delta",
		"delta": map[string]any{"type": deltaType, field: text}}})
}

func deltaOf(params json.RawMessage) string {
	var p struct {
		Delta string `json:"delta"`
	}
	json.Unmarshal(params, &p)
	return p.Delta
}

// joinSummary 拼接 reasoning 的 summary 段落（元素可能是字符串或 {text}）。
func joinSummary(parts []any) string {
	out := ""
	for _, p := range parts {
		var s string
		switch v := p.(type) {
		case string:
			s = v
		case map[string]any:
			s, _ = v["text"].(string)
		}
		if s == "" {
			continue
		}
		if out != "" {
			out += "\n\n"
		}
		out += s
	}
	return out
}
