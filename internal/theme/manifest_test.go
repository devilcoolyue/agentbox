package theme

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

func parseErr(t *testing.T, raw string) *ValidationError {
	t.Helper()
	_, err := Parse([]byte(raw))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("%s: want ValidationError, got %v", raw, err)
	}
	return ve
}

func TestParseNormalizesValidManifest(t *testing.T) {
	m, err := Parse([]byte(`{
		"agentbox_theme": 1, "id": "night-owl", "name": "  夜猫子  ", "base": "glass",
		"author": "Fixture", "description": "合成主题",
		"common": {"--radius-scale": "1.2", "--radius-pill": "999px", "--sans": "\"PingFang SC\",  sans-serif"},
		"dark": {
			"--bg": "#0b0e1c", "--panel": "rgba(22, 26, 46, .62)", "--accent": "oklch(70% 0.1 250)",
			"--field": "var( --bg )", "--panel-solid": "color-mix(in srgb, #1b1f36 90%, transparent)",
			"--shadow-md": "0 14px 40px rgba(0, 0, 0, .42), inset 0 1px 0 #fff",
			"--app-canvas": "radial-gradient(1200px 820px at 12% -12%,\n  rgba(139, 92, 246, .50), transparent 62%), linear-gradient(160deg, #0b0e1c 0%, #141838 100%)"
		},
		"light": {}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "夜猫子" || m.Light != nil || m.Common["--sans"] != `"PingFang SC", sans-serif` {
		t.Fatalf("not normalized: %+v", m)
	}
	if got := m.Dark["--app-canvas"]; strings.Contains(got, "\n") || strings.Contains(got, "  ") {
		t.Fatalf("whitespace kept: %q", got)
	}
}

func TestParseRejectsUnsafeValues(t *testing.T) {
	head := `{"agentbox_theme":1,"id":"x","name":"X","base":"amber","dark":{"--bg":`
	for _, value := range []string{
		`"url(https://evil.example/x)"`,
		`"URL(x)"`,
		`"u\\72l(x)"`,              // CSS escape
		`"image-set(\"x\" 1x)"`,    // quotes
		`"red; background: blue"`, // declaration break-out
		`"red} body{color:red"`,
		`"red /* c */"`,
		`"red !important"`,
		`"expression(alert(1))"`,
		`"element(#x)"`,
		`"var(--not-a-token)"`,
		`"var(--bg, url(x))"`,
		`"(1 2)"`,
		`"rgb(1, 2, 3"`,
		`"rgb(1, 2, 3))"`,
		`"#ggg"`,
		`"#12345"`,
		`"红色"`,
		`"rgb (1,2,3)"`,
		`"@import x"`,
		`""`,
		`"` + strings.Repeat("a", 201) + `"`,
		`"linear-gradient(red, blue)"`, // gradients only for --app-canvas
	} {
		if ve := parseErr(t, head+value+`}}`); ve.Code != "bad_value" || ve.Token != "--bg" || ve.Section != "dark" {
			t.Fatalf("%s: %+v", value, ve)
		}
	}
	for raw, token := range map[string]string{
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","common":{"--radius-scale":"4"}}`:          "--radius-scale",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","common":{"--radius-scale":"calc(1)"}}`:    "--radius-scale",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","common":{"--radius-pill":"1em"}}`:         "--radius-pill",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","common":{"--sans":"Comic Sans; x"}}`:      "--sans",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","common":{"--sans":"\"a\\\"b\""}}`:         "--sans",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","common":{"--sans":"微软雅黑"}}`:              "--sans",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","light":{"--app-canvas":"url(x)"}}`:        "--app-canvas",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","light":{"--app-canvas":"element(#app)"}}`: "--app-canvas",
	} {
		if ve := parseErr(t, raw); ve.Code != "bad_value" || ve.Token != token {
			t.Fatalf("%s: %+v", raw, ve)
		}
	}
}

func TestParseRejectsStructure(t *testing.T) {
	for raw, code := range map[string]string{
		`not json`: "bad_json",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber"} {}`:                         "bad_json",
		`{"agentbox_theme":2,"id":"x","name":"X","base":"amber"}`:                            "format",
		`{"id":"x","name":"X","base":"amber"}`:                                               "format",
		`{"agentbox_theme":1,"id":"X Y","name":"X","base":"amber"}`:                          "id",
		`{"agentbox_theme":1,"id":"-x","name":"X","base":"amber"}`:                           "id",
		`{"agentbox_theme":1,"id":"x","name":"","base":"amber"}`:                             "name",
		`{"agentbox_theme":1,"id":"x","name":"a\u0007","base":"amber"}`:                      "name",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"neon"}`:                             "base",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","css":"body{}"}`:             "unknown_field",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","dark":{"--claude":"red"}}`:  "unknown_token",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","dark":{"--dur":"0ms"}}`:     "unknown_token",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","dark":{"color":"red"}}`:     "unknown_token",
		`{"agentbox_theme":1,"id":"x","name":"X","base":"amber","dark":{"--bg":1}}`:          "bad_type",
		`{"agentbox_theme":1,"id":"x","name":"` + strings.Repeat("名", 41) + `","base":"amber"}`: "name",
	} {
		if ve := parseErr(t, raw); ve.Code != code {
			t.Fatalf("%s: got %+v want %s", raw, ve, code)
		}
	}
	big := `{"agentbox_theme":1,"id":"x","name":"X","base":"amber","description":"` + strings.Repeat("a", MaxBytes) + `"}`
	if ve := parseErr(t, big); ve.Code != "too_large" {
		t.Fatal(ve)
	}
}

// 每个风格 × 明暗都写全的令牌（skins.css 里出现十次的那批）必须能被主题改写，
// 否则新加的令牌在自定义主题里只能沿用底子风格的值，作者却改不动它。
func TestTokenAllowlistCoversSkinTokens(t *testing.T) {
	raw, err := os.ReadFile("../web/static/css/skins.css")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(--[a-z0-9-]+)\s*:`).FindAllStringSubmatch(string(raw), -1) {
		counts[m[1]]++
	}
	for name, n := range counts {
		if strings.HasPrefix(name, "--swatch-") {
			continue
		}
		if _, ok := tokenKinds[name]; !ok && n >= 2 {
			t.Errorf("%s is set by built-in styles but missing from the theme allowlist", name)
		}
	}
	base, err := os.ReadFile("../web/static/css/base.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range tokenList {
		if !strings.Contains(string(base), tok.Name+":") && counts[tok.Name] == 0 {
			if effects, _ := os.ReadFile("../web/static/css/skin-effects.css"); !strings.Contains(string(effects), tok.Name+":") {
				t.Errorf("%s is allowlisted but no stylesheet defines it", tok.Name)
			}
		}
	}
}

// docs/themes.md 里的示例必须能原样导入。
func TestDocumentedExampleParses(t *testing.T) {
	raw, err := os.ReadFile("../../docs/themes.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)```json\n(.*?)```").FindSubmatch(raw)
	if m == nil {
		t.Fatal("example missing from docs/themes.md")
	}
	if _, err := Parse(m[1]); err != nil {
		t.Fatal(err)
	}
}
