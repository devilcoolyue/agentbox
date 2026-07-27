package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const minimalConfig = `{
  "listen": "127.0.0.1:8180",
  "auth_token": "a-long-enough-token",
  "agent_image": "agentbox-agent:latest",
  "max_upload_mb": 10,
  "pricing": {
    "codex": {
      "input": 5, "cache_read": 0.5, "output": 30,
      "long_context_over": 272000,
      "long": {"input": 10, "cache_read": 1, "output": 45}
    },
    "gpt-5.4-mini": {"input": 0.75, "cache_read": 0.075, "output": 4.5}
  }
}`

func TestPriceLookupOrder(t *testing.T) {
	c := writeConfig(t, minimalConfig)

	// 精确模型名优先。
	if p, ok := c.Price("codex", "gpt-5.4-mini"); !ok || p.Input != 0.75 {
		t.Errorf("精确模型没命中: %+v ok=%v", p, ok)
	}
	// 事件不带模型名时按 agent 兜底——codex 的回合常常就是这样。
	if p, ok := c.Price("codex", ""); !ok || p.Input != 5 {
		t.Errorf("agent 兜底没命中: %+v ok=%v", p, ok)
	}
	// 表里没有的模型也走 agent 兜底。
	if p, ok := c.Price("codex", "gpt-9-unknown"); !ok || p.Input != 5 {
		t.Errorf("未知模型应回落到 agent: %+v ok=%v", p, ok)
	}
	// 查不到要能和「配成 0」区分开：调用方据此决定「不扣」而不是「免费」。
	if _, ok := c.Price("claude", "claude-opus-4-8"); ok {
		t.Error("claude 没配价，不该查到")
	}
}

// 长上下文是「过线整轮翻倍」，不是对超出部分加价；没配长档的模型永远用同一套价。
func TestRatesTierSelection(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	p, _ := c.Price("codex", "")

	if r := p.Rates(272_000); r.Input != 5 || r.Output != 30 {
		t.Errorf("阈值上应还是短上下文价: %+v", r)
	}
	if r := p.Rates(272_001); r.Input != 10 || r.Output != 45 {
		t.Errorf("过线应换成长上下文价: %+v", r)
	}

	mini, _ := c.Price("codex", "gpt-5.4-mini")
	if r := mini.Rates(10_000_000); r.Input != 0.75 {
		t.Errorf("没有长档的模型不该换价: %+v", r)
	}
}

// 配置里没有 pricing 时不能炸，也不该凭空造价。
func TestPriceMissingTable(t *testing.T) {
	c := writeConfig(t, `{"listen":"127.0.0.1:1","auth_token":"a-long-enough-token",
		"agent_image":"x","max_upload_mb":1}`)
	if _, ok := c.Price("codex", "gpt-5.5-codex"); ok {
		t.Error("没配价目表却查到了价")
	}
}

// 任何一次设置保存都会把整个配置重写回文件。工作副本漏抄字段 = 从文件里删掉它，
// 价目表被悄悄抹掉的话，codex 的消耗会从某一刻起全部按 0 计。
func TestSettingsSaveKeepsPricing(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	mode := "acceptEdits"
	if err := c.ApplySettings(SettingsPatch{PermissionMode: &mode}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		PermissionMode string                `json:"permission_mode"`
		Pricing        map[string]ModelPrice `json:"pricing"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.PermissionMode != "acceptEdits" {
		t.Errorf("设置没存上: %q", onDisk.PermissionMode)
	}
	if len(onDisk.Pricing) != 2 || onDisk.Pricing["codex"].Input != 5 {
		t.Fatalf("保存设置把价目表弄丢了: %+v", onDisk.Pricing)
	}
	// 长上下文那一档是嵌套结构，最容易在序列化里掉；掉了会让大回合按半价扣。
	long := onDisk.Pricing["codex"].Long
	if long == nil || long.Input != 10 || onDisk.Pricing["codex"].LongContextOver != 272_000 {
		t.Errorf("长上下文档没存住: over=%d long=%+v",
			onDisk.Pricing["codex"].LongContextOver, long)
	}
	// 内存里的那份也要还在，否则要等到重启才恢复。
	if p, ok := c.Price("codex", ""); !ok || p.Input != 5 || p.Rates(300_000).Input != 10 {
		t.Errorf("内存中的价目表没了: %+v ok=%v", p, ok)
	}
}
