package agent

// Codex app-server 条目的富字段。exec --json 的 item.* 形状只有命令串与路径，
// 网页上每条命令都只剩一行「exec /bin/bash -lc '…'」，看不出做了什么、成没成；
// CLI 自己显示的那些——命令归类成读/搜/列、退出码与耗时、输出首尾几行、改动的
// 增删行数与 diff、MCP / 网页搜索 / 看图 / 子 Agent 等工具——v2 的 ThreadItem
// 里都有。这里按 snake_case 追加进落盘事件：exec 回退路径、旧前端不认识这些
// 字段，照旧渲染。
//
// 落盘有界：命令输出只留首尾各若干行（CLI 折叠显示也只给首尾），diff 与工具
// 结果按字节截断，免得一次 cat 大文件吃掉历史分页的字节预算。完整内容仍在
// 会话 home 的 rollout 里。
//
// 解码用逐字段的 rawItem 而不是一个大结构体：哪个字段换了类型只丢那一个字段，
// 不会让整条条目解码失败、从对话里消失。

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	outputSideLines  = 20       // 命令输出首、尾各留的行数
	outputSideBytes  = 3 << 10  // 首、尾各自的字节上限
	diffChangeBytes  = 8 << 10  // 单个文件 diff 的字节上限
	diffItemBytes    = 24 << 10 // 一次改动全部 diff 的字节上限
	toolTextBytes    = 4 << 10  // 工具结果正文
	toolArgsBytes    = 2 << 10  // 工具入参（压成一行 JSON）
	toolPromptBytes  = 1 << 10  // 子 Agent 任务描述
	liveOutputBytes  = 64 << 10 // 单条命令实时转发的输出上限，之后只等收尾预览
	maxSearchResults = 8
)

// rawItem 是一条 ThreadItem 的字段表，取值失败一律当缺省。
type rawItem map[string]json.RawMessage

func (m rawItem) str(k string) string {
	var s string
	json.Unmarshal(m[k], &s)
	return s
}

// path 取路径字段；v2 的 LegacyAppPathString 有时带 file:// 前缀。
func (m rawItem) path(k string) string {
	return strings.TrimPrefix(m.str(k), "file://")
}

func (m rawItem) num(k string) (int64, bool) {
	var n *int64
	if json.Unmarshal(m[k], &n) != nil || n == nil {
		return 0, false
	}
	return *n, true
}

func (m rawItem) flag(k string) (bool, bool) {
	var b *bool
	if json.Unmarshal(m[k], &b) != nil || b == nil {
		return false, false
	}
	return *b, true
}

// set 只在值非空时写入，事件里不出现一堆 null / 空串。
func set(ev map[string]any, k string, v any) {
	switch x := v.(type) {
	case string:
		if x == "" {
			return
		}
	case []map[string]any:
		if len(x) == 0 {
			return
		}
	case nil:
		return
	}
	ev[k] = v
}

func (m rawItem) setInt(ev map[string]any, dst, src string) {
	if n, ok := m.num(src); ok {
		ev[dst] = n
	}
}

// itemStatus 把 v2 的 camelCase 状态换成 exec --json 的写法（inProgress → in_progress）。
func itemStatus(s string) string {
	if s == "inProgress" {
		return "in_progress"
	}
	return s
}

func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// clip 按字节截断且不切断 UTF-8 字符。
func clip(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// clipTail 保留末尾至多 n 字节。
func clipTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// commandActions 是 CLI 对命令串的解析（读文件 / 列目录 / 搜索 / 其它），
// 网页据此把 `sed -n 1,80p a.go` 显示成「Read a.go」，与 CLI 的 Explored 分组同源。
func commandActions(raw json.RawMessage) []map[string]any {
	var acts []rawItem
	if json.Unmarshal(raw, &acts) != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(acts))
	for _, a := range acts {
		t := a.str("type")
		if t == "" {
			continue
		}
		m := map[string]any{"type": snakeCase(t)}
		for _, k := range []string{"command", "name", "query"} {
			set(m, k, a.str(k))
		}
		set(m, "path", a.path("path"))
		out = append(out, m)
	}
	return out
}

// outputPreview 把命令输出压成首尾各 outputSideLines 行；截掉时 head 进
// aggregated_output、tail 进 output_tail，前端在中间标出省略了多少行。
// 确实没有输出时记 output_lines=0，与「没记录输出」（旧记录）区分开。
func outputPreview(ev map[string]any, raw json.RawMessage) {
	var out *string
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return
	}
	if strings.TrimSpace(*out) == "" {
		ev["output_lines"] = 0
		return
	}
	trimmed := strings.TrimRight(*out, "\n")
	lines := strings.Count(trimmed, "\n") + 1
	ev["output_lines"] = lines
	if lines <= 2*outputSideLines && len(trimmed) <= 2*outputSideBytes {
		ev["aggregated_output"] = trimmed
		return
	}
	all := strings.Split(trimmed, "\n")
	headN := min(outputSideLines, len(all))
	head, _ := clip(strings.Join(all[:headN], "\n"), outputSideBytes)
	tailN := min(outputSideLines, len(all)-headN)
	tail := clipTail(strings.Join(all[len(all)-tailN:], "\n"), outputSideBytes)
	ev["aggregated_output"] = head
	set(ev, "output_tail", tail)
	ev["output_truncated"] = true
}

func (c *appServerConn) commandEvent(it rawItem, completed bool) map[string]any {
	ev := map[string]any{"type": "command_execution", "command": it.str("command")}
	set(ev, "id", it.str("id"))
	set(ev, "actions", commandActions(it["commandActions"]))
	set(ev, "cwd", it.path("cwd"))
	if src := it.str("source"); src != "" && src != "agent" {
		ev["source"] = snakeCase(src)
	}
	if !completed {
		return ev
	}
	set(ev, "status", itemStatus(it.str("status")))
	it.setInt(ev, "exit_code", "exitCode")
	it.setInt(ev, "duration_ms", "durationMs")
	outputPreview(ev, it["aggregatedOutput"])
	return ev
}

// diffStat 数 unified diff 的增删行；新建 / 删除文件时 diff 就是全文。
func diffStat(kind, diff string) (added, removed int) {
	if diff == "" {
		return 0, 0
	}
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	switch kind {
	case "add":
		return len(lines), 0
	case "delete":
		return 0, len(lines)
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			added++
		case strings.HasPrefix(l, "-"):
			removed++
		}
	}
	return
}

func (c *appServerConn) fileChangeEvent(it rawItem) map[string]any {
	var changes []rawItem
	json.Unmarshal(it["changes"], &changes)
	budget := diffItemBytes
	out := make([]map[string]any, 0, len(changes))
	for _, ch := range changes {
		var kind rawItem
		json.Unmarshal(ch["kind"], &kind)
		k := kind.str("type")
		diff := ch.str("diff")
		added, removed := diffStat(k, diff)
		m := map[string]any{"path": ch.str("path"), "added": added, "removed": removed}
		set(m, "kind", k)
		set(m, "move_path", kind.str("move_path"))
		if diff != "" {
			kept, cut := clip(diff, min(diffChangeBytes, budget))
			budget -= len(kept)
			set(m, "diff", kept)
			if cut {
				m["diff_truncated"] = true
			}
		}
		out = append(out, m)
	}
	ev := map[string]any{"type": "file_change", "changes": out}
	set(ev, "id", it.str("id"))
	set(ev, "status", itemStatus(it.str("status")))
	return ev
}

// compactJSON 把工具入参压成一行并截断；本身就是字符串时原样截断。
func compactJSON(raw json.RawMessage, n int) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s, _ = clip(s, n)
		return s
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	b, _ := json.Marshal(v)
	s, cut := clip(string(b), n)
	if cut {
		s += "…"
	}
	return s
}

// contentText 取 MCP / 动态工具结果里的文字段落；图片等只记一个占位。
func contentText(raw json.RawMessage) string {
	var items []rawItem
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var parts []string
	for _, ci := range items {
		switch t := ci.str("type"); t {
		case "text", "inputText":
			if s := ci.str("text"); s != "" {
				parts = append(parts, s)
			}
		case "":
		default:
			parts = append(parts, "["+snakeCase(t)+"]")
		}
	}
	s, cut := clip(strings.Join(parts, "\n"), toolTextBytes)
	if cut {
		s += "…"
	}
	return s
}

func (c *appServerConn) mcpEvent(it rawItem, completed bool) map[string]any {
	ev := map[string]any{"type": "mcp_tool_call", "server": it.str("server"), "tool": it.str("tool")}
	set(ev, "id", it.str("id"))
	set(ev, "arguments", compactJSON(it["arguments"], toolArgsBytes))
	if !completed {
		return ev
	}
	set(ev, "status", itemStatus(it.str("status")))
	it.setInt(ev, "duration_ms", "durationMs")
	var res rawItem
	if json.Unmarshal(it["result"], &res) == nil {
		set(ev, "result_text", contentText(res["content"]))
	}
	var e rawItem
	if json.Unmarshal(it["error"], &e) == nil {
		if msg := e.str("message"); msg != "" {
			ev["error"] = map[string]any{"message": msg}
		}
	}
	return ev
}

func (c *appServerConn) dynamicToolEvent(it rawItem, completed bool) map[string]any {
	ev := map[string]any{"type": "dynamic_tool_call", "tool": it.str("tool")}
	set(ev, "id", it.str("id"))
	set(ev, "namespace", it.str("namespace"))
	set(ev, "arguments", compactJSON(it["arguments"], toolArgsBytes))
	if !completed {
		return ev
	}
	set(ev, "status", itemStatus(it.str("status")))
	it.setInt(ev, "duration_ms", "durationMs")
	if ok, has := it.flag("success"); has {
		ev["success"] = ok
	}
	set(ev, "result_text", contentText(it["contentItems"]))
	return ev
}

func (c *appServerConn) webSearchEvent(it rawItem, completed bool) map[string]any {
	ev := map[string]any{"type": "web_search", "query": it.str("query")}
	set(ev, "id", it.str("id"))
	var act rawItem
	if json.Unmarshal(it["action"], &act) == nil && act.str("type") != "" {
		a := map[string]any{"type": snakeCase(act.str("type"))}
		for _, k := range []string{"query", "url", "pattern"} {
			set(a, k, act.str(k))
		}
		var qs []string
		if json.Unmarshal(act["queries"], &qs) == nil && len(qs) > 0 {
			a["queries"] = qs
		}
		ev["action"] = a
	}
	if !completed {
		return ev
	}
	var results []rawItem
	if json.Unmarshal(it["results"], &results) == nil {
		var out []map[string]any
		for _, r := range results {
			if len(out) == maxSearchResults {
				break
			}
			m := map[string]any{}
			for _, k := range []string{"title", "url", "domain"} {
				set(m, k, r.str(k))
			}
			if len(m) > 0 {
				out = append(out, m)
			}
		}
		set(ev, "results", out)
		if len(results) > 0 {
			ev["result_count"] = len(results)
		}
	}
	return ev
}

func (c *appServerConn) collabEvent(it rawItem, completed bool) map[string]any {
	ev := map[string]any{"type": "collab_agent_tool_call", "tool": snakeCase(it.str("tool"))}
	set(ev, "id", it.str("id"))
	set(ev, "model", it.str("model"))
	if p, cut := clip(it.str("prompt"), toolPromptBytes); p != "" {
		if cut {
			p += "…"
		}
		ev["prompt"] = p
	}
	var receivers []string
	if json.Unmarshal(it["receiverThreadIds"], &receivers) == nil && len(receivers) > 0 {
		ev["receivers"] = len(receivers)
	}
	if completed {
		set(ev, "status", itemStatus(it.str("status")))
	}
	return ev
}

// richItem 翻译 agentMessage / reasoning 以外的条目；返回 nil 表示这类条目
// 对网页没有可显示的内容（用户消息回显、hook 提示等）。started 只给耗时
// 明显、值得先亮一行「进行中」的几类。
func (c *appServerConn) richItem(completed bool, typ string, it rawItem) map[string]any {
	switch typ {
	case "commandExecution":
		return c.commandEvent(it, completed)
	case "mcpToolCall":
		return c.mcpEvent(it, completed)
	case "dynamicToolCall":
		return c.dynamicToolEvent(it, completed)
	case "webSearch":
		return c.webSearchEvent(it, completed)
	case "collabAgentToolCall":
		return c.collabEvent(it, completed)
	}
	if !completed {
		return nil
	}
	ev := map[string]any{}
	switch typ {
	case "fileChange":
		return c.fileChangeEvent(it)
	case "imageView":
		ev["type"] = "image_view"
		set(ev, "path", it.path("path"))
	case "imageGeneration":
		ev["type"] = "image_generation"
		set(ev, "status", itemStatus(it.str("status")))
		set(ev, "revised_prompt", it.str("revisedPrompt"))
		set(ev, "saved_path", it.path("savedPath"))
		var f rawItem
		if json.Unmarshal(it["failure"], &f) == nil {
			set(ev, "failure", snakeCase(f.str("type")))
		}
	case "contextCompaction":
		ev["type"] = "context_compaction"
	case "subAgentActivity":
		ev["type"] = "sub_agent_activity"
		set(ev, "kind", it.str("kind"))
		set(ev, "agent_path", it.str("agentPath"))
		set(ev, "model", it.str("model"))
	case "plan":
		ev["type"] = "plan"
		set(ev, "text", it.str("text"))
	case "enteredReviewMode", "exitedReviewMode":
		ev["type"] = "review_mode"
		ev["entered"] = typ == "enteredReviewMode"
		set(ev, "review", it.str("review"))
	case "sleep":
		ev["type"] = "sleep"
		it.setInt(ev, "duration_ms", "durationMs")
	default:
		return nil
	}
	set(ev, "id", it.str("id"))
	return ev
}

// outputDelta 转发命令的实时输出：只广播不落盘（stream_event），每条命令
// 最多转发 liveOutputBytes，之后网页等收尾时的首尾预览。
func (c *appServerConn) outputDelta(params json.RawMessage) {
	var p struct {
		ItemID string `json:"itemId"`
		Delta  string `json:"delta"`
	}
	if json.Unmarshal(params, &p) != nil || p.ItemID == "" || p.Delta == "" {
		return
	}
	if c.liveOut == nil {
		c.liveOut = map[string]int{}
	}
	room := liveOutputBytes - c.liveOut[p.ItemID]
	if room <= 0 {
		return
	}
	delta, _ := clip(p.Delta, room)
	c.liveOut[p.ItemID] += len(delta)
	c.emitJSON(map[string]any{"type": "stream_event", "event": map[string]any{
		"type": "item_output_delta", "item_id": p.ItemID, "delta": delta}})
}

func (c *appServerConn) toolProgress(params json.RawMessage) {
	var p struct {
		ItemID  string `json:"itemId"`
		Message string `json:"message"`
	}
	if json.Unmarshal(params, &p) != nil || p.ItemID == "" || p.Message == "" {
		return
	}
	msg, _ := clip(p.Message, toolPromptBytes)
	c.emitJSON(map[string]any{"type": "stream_event", "event": map[string]any{
		"type": "item_progress", "item_id": p.ItemID, "message": msg}})
}
