package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePluginSource(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want pluginSource
	}{
		{"仓库内相对路径", `"./plugins/agent-sdk-dev"`, pluginSource{Local: true, Path: "plugins/agent-sdk-dev"}},
		{"git-subdir", `{"source":"git-subdir","url":"https://github.com/adobe/skills.git","path":"plugins/x","ref":"main"}`,
			pluginSource{URL: "https://github.com/adobe/skills.git", Ref: "main", Path: "plugins/x"}},
		{"url", `{"source":"url","url":"https://github.com/endorlabs/ai-plugins.git","sha":"deadbeef"}`,
			pluginSource{URL: "https://github.com/endorlabs/ai-plugins.git"}},
		{"github", `{"source":"github","repo":"jfrog/claude-plugin","commit":"259c8e718266c16e99b4f30ae9b1ed0f9f00d98d"}`,
			pluginSource{URL: "https://github.com/jfrog/claude-plugin", Ref: "259c8e718266c16e99b4f30ae9b1ed0f9f00d98d"}},
	}
	for _, c := range cases {
		got, err := parsePluginSource(json.RawMessage(c.raw))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// 目录数据来自网络，路径字段必须当不可信输入处理。
func TestParsePluginSourceRejectsUnsafe(t *testing.T) {
	for _, raw := range []string{
		`"../../etc"`,
		`"/etc/passwd"`,
		`{"source":"git-subdir","url":"https://github.com/x/y.git","path":"../../../etc"}`,
		`{"source":"url","url":"file:///etc"}`,
		`{"source":"url","url":"git@github.com:x/y.git"}`,
	} {
		if _, err := parsePluginSource(json.RawMessage(raw)); err == nil {
			t.Fatalf("接受了不安全的来源: %s", raw)
		}
	}
}

func TestDiscoverSkillDirs(t *testing.T) {
	mkSkill := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, skillManifest), []byte(demoSkill), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 约定式：skills/<名字>/SKILL.md
	conv := t.TempDir()
	mkSkill(filepath.Join(conv, "skills", "alpha"))
	mkSkill(filepath.Join(conv, "skills", "beta"))
	if err := os.MkdirAll(filepath.Join(conv, "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := discoverSkillDirs(conv, nil); len(got) != 2 {
		t.Fatalf("约定式: got %v, want 2", got)
	}

	// 声明式：目录里写死的相对路径优先
	decl := t.TempDir()
	mkSkill(filepath.Join(decl, "local-ai-use"))
	mkSkill(filepath.Join(decl, "skills", "ignored"))
	got := discoverSkillDirs(decl, []string{"./local-ai-use", "../escape", "/etc"})
	if len(got) != 1 || filepath.Base(got[0]) != "local-ai-use" {
		t.Fatalf("声明式: got %v", got)
	}

	// 插件本身就是一个技能
	single := t.TempDir()
	mkSkill(single)
	if got := discoverSkillDirs(single, nil); len(got) != 1 || got[0] != single {
		t.Fatalf("单技能插件: got %v", got)
	}

	// 纯命令插件：什么都不该找到
	cmds := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cmds, "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := discoverSkillDirs(cmds, nil); len(got) != 0 {
		t.Fatalf("命令类插件: got %v, want 0", got)
	}
}

// TestMarketLive 真的去 GitHub 拉一次官方目录并装一个一方插件的技能，验证
// 抓取链路与真实数据的兼容性。依赖外网，默认跳过：
// MARKET_LIVE_TEST=1 go test -run TestMarketLive ./internal/server/
func TestMarketLive(t *testing.T) {
	if os.Getenv("MARKET_LIVE_TEST") == "" {
		t.Skip("需要 MARKET_LIVE_TEST=1（会克隆 GitHub 仓库）")
	}
	s, sess := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/marketplace", nil)
	w := httptest.NewRecorder()
	s.handleMarketList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", w.Code, w.Body.String())
	}
	var cat struct {
		Plugins    []marketEntry `json:"plugins"`
		Categories []string      `json:"categories"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Plugins) < 50 || len(cat.Categories) < 3 {
		t.Fatalf("目录看起来不对: %d 个插件 / %d 个分类", len(cat.Plugins), len(cat.Categories))
	}

	// frontend-design 是一方插件（仓库内相对路径），且带 skills/
	ireq := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/skills/market?scope=template",
		strings.NewReader(`{"name":"frontend-design"}`))
	iw := httptest.NewRecorder()
	s.handleSkillMarketInstall(iw, ireq, sess)
	if iw.Code != http.StatusOK {
		t.Fatalf("install status = %d: %s", iw.Code, iw.Body.String())
	}
	var res struct {
		Installed []string `json:"installed"`
	}
	if err := json.Unmarshal(iw.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Installed) == 0 {
		t.Fatal("没装上任何技能")
	}
	p := filepath.Join(s.userTemplateDir(sess.User), ".claude", "skills", res.Installed[0], skillManifest)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s 不在: %v", p, err)
	}

	// 纯命令插件要给出明确的 422，而不是装出个空技能
	creq := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/skills/market",
		strings.NewReader(`{"name":"code-review"}`))
	cw := httptest.NewRecorder()
	s.handleSkillMarketInstall(cw, creq, sess)
	if cw.Code != http.StatusUnprocessableEntity {
		t.Fatalf("命令类插件 status = %d: %s", cw.Code, cw.Body.String())
	}
	if !strings.Contains(cw.Body.String(), "claude plugin install") {
		t.Fatalf("没给出改用终端的提示: %s", cw.Body.String())
	}
}

// 缓存新鲜时不该再去网络。
func TestMarketRepoUsesCache(t *testing.T) {
	s, _ := newTestServer(t)
	repo := s.marketRepo()
	if err := os.MkdirAll(filepath.Join(repo, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude-plugin", "marketplace.json"),
		[]byte(`{"plugins":[{"name":"x","description":"d","source":"./plugins/x"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(s.marketDir(), "fetched-at")
	if err := os.WriteFile(stamp, []byte("now"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	entries, _, err := s.readMarket(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "x" {
		t.Fatalf("entries = %+v", entries)
	}
}
