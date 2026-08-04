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

/* ---- IP 代理池 ---- */

const proxyConfig = `{
  "listen": "127.0.0.1:8180",
  "auth_token": "a-long-enough-token",
  "agent_image": "agentbox-agent:latest",
  "max_upload_mb": 10,
  "proxies": [
    {"id": "px1", "name": "香港", "scheme": "socks5", "host": "1.2.3.4", "port": 1080,
     "username": "u", "password": "p"},
    {"id": "px2", "scheme": "http", "host": "5.6.7.8", "port": 8080, "disabled": true}
  ],
  "accounts": [
    {"id": "claude-1", "type": "claude", "label": "A", "proxy_id": "px1"},
    {"id": "codex-1", "type": "codex", "label": "B"}
  ]
}`

func TestProxyLoadAndAccountBinding(t *testing.T) {
	c := writeConfig(t, proxyConfig)

	p, bound := c.AccountProxy("claude-1")
	if !bound || p.ID != "px1" || p.Username != "u" {
		t.Fatalf("绑定的代理没读出来: %+v bound=%v", p, bound)
	}
	if got := p.URL(); got != "socks5://u:p@1.2.3.4:1080" {
		t.Errorf("URL() = %q", got)
	}
	// 展示用的地址不能带凭证——它会出现在日志、错误信息和账号列表里。
	if got := p.DisplayURL(); got != "socks5://1.2.3.4:1080" {
		t.Errorf("DisplayURL() = %q，不该含用户名密码", got)
	}
	if _, bound := c.AccountProxy("codex-1"); bound {
		t.Error("没写 proxy_id 的账号不该被认为绑定了代理")
	}
	// 名称留空时用主机名兜底，列表里才不会出现一片空白行。
	if p2, _ := c.Proxy("px2"); p2.Name != "5.6.7.8" {
		t.Errorf("空名称没回落到主机名: %q", p2.Name)
	}
	if got := c.GetProxyBridge().Bind; got != defaultProxyBridgeBind {
		t.Errorf("桥接默认绑定 = %q, want %q", got, defaultProxyBridgeBind)
	}
	if host := c.GetProxyBridge().Host; host != "172.17.0.1" {
		t.Errorf("桥接 host 应从 bind 推出来, got %q", host)
	}
}

func TestProxyValidationRejectsDanglingBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"listen":"127.0.0.1:1","auth_token":"a-long-enough-token",
	  "agent_image":"x","max_upload_mb":10,
	  "accounts":[{"id":"a1","type":"claude","label":"A","proxy_id":"nope"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("指向不存在代理的 proxy_id 应该校验失败")
	}
}

// 删代理必须同时解绑账号：留着悬空的 proxy_id 会让下一次任何配置改动都卡在
// 校验上——那时候管理员根本不知道是哪一步埋的雷。
func TestRemoveProxyClearsAccountBinding(t *testing.T) {
	c := writeConfig(t, proxyConfig)
	if err := c.RemoveProxy("px1"); err != nil {
		t.Fatal(err)
	}
	if _, bound := c.AccountProxy("claude-1"); bound {
		t.Error("代理删掉后账号还留着绑定")
	}
	// 重新加载落盘结果，确认写回的文件本身也是自洽的。
	c2, err := Load(c.Path())
	if err != nil {
		t.Fatalf("删除后写回的配置无法重新加载: %v", err)
	}
	if len(c2.ProxyList()) != 1 {
		t.Errorf("剩余代理数 = %d, want 1", len(c2.ProxyList()))
	}
}

// mutate 的工作副本漏抄字段就等于把它从 config.json 里删掉，这里盯住代理池和
// 账号绑定这两个新字段。
func TestMutatePreservesProxies(t *testing.T) {
	c := writeConfig(t, proxyConfig)
	if err := c.SetAuthToken("another-long-token"); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Proxies) != 2 {
		t.Fatalf("无关改动后代理池被写没了: %d", len(c2.Proxies))
	}
	if p, bound := c2.AccountProxy("claude-1"); !bound || p.Password != "p" {
		t.Errorf("账号绑定或代理密码丢失: %+v bound=%v", p, bound)
	}
}

func TestUpdateProxyKeepsUntouchedFields(t *testing.T) {
	c := writeConfig(t, proxyConfig)
	name := "东京"
	if _, err := c.UpdateProxy("px1", ProxyPatch{Name: &name}); err != nil {
		t.Fatal(err)
	}
	p, _ := c.Proxy("px1")
	if p.Name != "东京" || p.Password != "p" || p.Port != 1080 {
		t.Errorf("只改名字却动了别的字段: %+v", p)
	}
}

func TestAddProxiesSkipsExistingIDs(t *testing.T) {
	c := writeConfig(t, proxyConfig)
	n, err := c.AddProxies([]Proxy{
		{ID: "px1", Scheme: "socks5", Host: "9.9.9.9", Port: 1080},
		{ID: "px3", Scheme: "socks5", Host: "9.9.9.9", Port: 1080},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("added = %d, want 1", n)
	}
	if p, _ := c.Proxy("px1"); p.Host != "1.2.3.4" {
		t.Errorf("同 id 的导入项不该覆盖已有代理: %+v", p)
	}
}
