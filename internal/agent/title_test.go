package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"agentbox/internal/config"
)

// claude -p --output-format json 的单对象输出：标题在 result 里，同一个对象
// 顺带带着 usage/modelUsage/total_cost_usd。数值形状取自生产 transcript。
const claudeTitleJSON = `{"type":"result","subtype":"success","is_error":false,` +
	`"duration_ms":1842,"num_turns":1,"result":"修复备份丢 WAL 的问题",` +
	`"session_id":"35842e7b-be38-4a0f-a851-66046194c5d6","total_cost_usd":0.000621,` +
	`"usage":{"input_tokens":531,"output_tokens":18,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},` +
	`"modelUsage":{"claude-haiku-4-5-20251001":{"inputTokens":531,"outputTokens":18,` +
	`"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.000621}}}`

func TestTitleOutputClaude(t *testing.T) {
	title, usage := TitleOutput(config.AgentClaude, claudeTitleJSON+"\n")
	if title != "修复备份丢 WAL 的问题" {
		t.Errorf("标题 = %q", title)
	}
	if len(usage) == 0 {
		t.Fatal("应带出用量事件，否则起标题的消耗又统计不到了")
	}
	// 带出的必须是能直接喂给用量解析器的 result 事件。
	var probe struct {
		Type    string  `json:"type"`
		CostUSD float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(usage, &probe); err != nil {
		t.Fatalf("用量事件不是合法 JSON: %v", err)
	}
	if probe.Type != "result" || probe.CostUSD != 0.000621 {
		t.Errorf("用量事件形状不对: %+v", probe)
	}
}

// 旧版 CLI 不认 --output-format json 时会吐纯文本，此时仍要拿到可用标题。
func TestTitleOutputClaudePlainTextFallback(t *testing.T) {
	title, usage := TitleOutput(config.AgentClaude, "就是个普通标题\n")
	if title != "就是个普通标题\n" {
		t.Errorf("纯文本回退失败: %q", title)
	}
	if usage != nil {
		t.Error("纯文本里没有用量，不该编一个出来")
	}
}

// 半截 JSON（进程被 kill 等）同样按纯文本处理，不能 panic 也不能吞掉标题。
func TestTitleOutputClaudeTruncatedJSON(t *testing.T) {
	const broken = `{"type":"result","result":"半截`
	title, usage := TitleOutput(config.AgentClaude, broken)
	if title != broken || usage != nil {
		t.Errorf("截断 JSON 应原样回退: title=%q usage=%v", title, usage)
	}
}

// codex 标题命令的真实输出：标题、分隔符、turn.completed 各占一行。
// 整段取自容器里跑 TitleCommand(codex) 的实测结果，未经改写。
const codexTitleStdout = "排查空数据库备份\n---abox-usage---\n" +
	`{"type":"turn.completed","usage":{"input_tokens":10523,"cached_input_tokens":8576,` +
	`"cache_write_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0}}` + "\n"

func TestTitleOutputCodex(t *testing.T) {
	title, usage := TitleOutput(config.AgentCodex, codexTitleStdout)
	if strings.TrimSpace(title) != "排查空数据库备份" {
		t.Errorf("标题 = %q", title)
	}
	if len(usage) == 0 {
		t.Fatal("应带出 turn.completed，否则 codex 起标题的消耗还是漏账")
	}
	var probe struct {
		Type  string `json:"type"`
		Usage struct {
			Input int64 `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(usage, &probe); err != nil {
		t.Fatalf("用量事件不是合法 JSON: %v", err)
	}
	if probe.Type != "turn.completed" || probe.Usage.Input != 10523 {
		t.Errorf("用量事件形状不对: %+v", probe)
	}
}

// 标题里正好出现分隔符时，从后往前找，用量行不能被吞掉。
func TestTitleOutputCodexMarkerInTitle(t *testing.T) {
	const s = "聊聊 ---abox-usage--- 这个分隔符\n---abox-usage---\n" +
		`{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":1}}` + "\n"
	title, usage := TitleOutput(config.AgentCodex, s)
	if strings.TrimSpace(title) != "聊聊 ---abox-usage--- 这个分隔符" {
		t.Errorf("标题被截错: %q", title)
	}
	if len(usage) == 0 {
		t.Error("用量行被吞了")
	}
}

// codex 回合失败等情况下没有 turn.completed，grep 出空行；此时仍要给出标题。
func TestTitleOutputCodexNoUsageLine(t *testing.T) {
	title, usage := TitleOutput(config.AgentCodex, "还是有标题的\n---abox-usage---\n\n")
	if strings.TrimSpace(title) != "还是有标题的" {
		t.Errorf("标题 = %q", title)
	}
	if usage != nil {
		t.Error("没有用量行时不该编一个出来")
	}
}

// 完全没有分隔符（旧版镜像里的老脚本）时按纯文本处理。
func TestTitleOutputCodexLegacyPlainText(t *testing.T) {
	title, usage := TitleOutput(config.AgentCodex, "老脚本只吐标题")
	if title != "老脚本只吐标题" || usage != nil {
		t.Errorf("旧格式回退失败: title=%q usage=%v", title, usage)
	}
}

// TitleCommand 必须让 claude 走 json 输出，否则 TitleOutput 永远拿不到用量。
func TestTitleCommandClaudeUsesJSON(t *testing.T) {
	cmd, err := TitleCommand(config.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	var format string
	for i, a := range cmd {
		if a == "--output-format" && i+1 < len(cmd) {
			format = cmd[i+1]
		}
	}
	if format != "json" {
		t.Errorf("--output-format = %q, 想要 json（text 会丢掉用量）", format)
	}
}
