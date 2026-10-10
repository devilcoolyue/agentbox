// Package theme 管理用户可导入的界面主题清单。
//
// 主题不是一份任意 CSS，而是「基于某个内置风格 + 改写一组白名单令牌」：
// 内置风格（data-skin）照旧提供形状与专属特效，清单只换 base.css / skins.css
// 里那批颜色、投影、圆角、字体令牌的取值。控制台没有 CSP、登录令牌又放在
// localStorage，任意 CSS 能用 url() 外连、用属性选择器探测页面、伪造按钮，
// 所以这里只收白名单里的令牌名，取值按种类走严格语法：不允许引号以外的
// 转义、分号、花括号，函数名逐个对白名单，url()/image-set() 之类一律进不来。
// 全站主题由管理员导入、所有用户可选；个人主题只给本人用。
package theme

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FormatVersion 是清单里 agentbox_theme 字段的取值；不认识的版本直接拒绝，
// 不猜新格式的语义。
const FormatVersion = 1

const (
	// MaxBytes 是单个清单文件的上限；写全两套令牌的清单约 6 KiB。
	MaxBytes      = 64 << 10
	maxNameRunes  = 40
	maxAuthor     = 60
	maxDesc       = 200
	maxValue      = 200
	maxCanvas     = 1200
	maxFont       = 300
	maxFontFamily = 12
	maxParenDepth = 6
)

// Bases 是可作为底子的内置风格，与 web/src/theme.ts 的 SKINS 一致。
var Bases = []string{"amber", "glass", "cyberpunk", "graphite", "verdant", "blueprint"}

// Kind 决定令牌取值按哪种语法校验。
type Kind string

const (
	KindColor  Kind = "color"  // 颜色：#hex、rgb()/oklch()/color-mix() 等，或 var(--白名单令牌)
	KindShadow Kind = "shadow" // box-shadow 列表：长度 + 颜色 + inset
	KindCanvas Kind = "canvas" // 页面底层背景：颜色或渐变
	KindScale  Kind = "scale"  // 圆角倍数（纯数字）
	KindRadius Kind = "radius" // 胶囊/圆形圆角：px 或 %
	KindFont   Kind = "font"   // font-family 列表
)

// Token 是一个可改写的令牌。
type Token struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
}

// tokenList 与 internal/web/static/css/base.css、skins.css 的令牌对应；
// 新增「十套都要补」的颜色令牌时一并加进来，tokens_test.go 会核对。
// 动效（--dur-*、--ease-*）不在内：「减少动态效果」要能把它们统一归零。
// 品牌色（--claude、--openai）与风格色卡（--swatch-*）也不开放。
var tokenList = []Token{
	{"--bg", KindColor}, {"--panel", KindColor}, {"--panel-2", KindColor}, {"--panel-solid", KindColor},
	{"--line", KindColor}, {"--line-soft", KindColor},
	{"--text", KindColor}, {"--text-hi", KindColor}, {"--muted", KindColor},
	{"--scroll-thumb", KindColor}, {"--scroll-thumb-hover", KindColor}, {"--scroll-thumb-active", KindColor},
	{"--amber", KindColor}, {"--amber-dim", KindColor},
	{"--accent", KindColor}, {"--accent-hi", KindColor}, {"--on-accent", KindColor},
	{"--green", KindColor}, {"--green-line", KindColor},
	{"--red", KindColor}, {"--red-hi", KindColor}, {"--red-line", KindColor}, {"--on-red", KindColor},
	{"--warn", KindColor}, {"--warn-line", KindColor},
	{"--code-bg", KindColor}, {"--code-head", KindColor}, {"--code-line", KindColor},
	{"--syntax-keyword", KindColor}, {"--syntax-function", KindColor},
	{"--term-bg", KindColor}, {"--term-fg", KindColor}, {"--term-cursor", KindColor}, {"--term-sel", KindColor},
	{"--scrim", KindColor}, {"--backdrop", KindColor}, {"--backdrop-strong", KindColor}, {"--overlay-chip", KindColor},
	{"--shadow-sm", KindShadow}, {"--shadow-md", KindShadow}, {"--shadow-lg", KindShadow},
	{"--checker-a", KindColor}, {"--checker-b", KindColor},
	{"--diff-add", KindColor}, {"--info", KindColor}, {"--busy-veil", KindColor},
	{"--field", KindColor}, {"--page", KindColor},
	{"--app-canvas", KindCanvas},
	{"--skin-detail", KindColor}, {"--glass-glint", KindColor}, {"--glass-edge", KindColor},
	{"--radius-scale", KindScale}, {"--radius-pill", KindRadius}, {"--radius-round", KindRadius},
	{"--sans", KindFont}, {"--mono", KindFont},
}

var tokenKinds = func() map[string]Kind {
	m := make(map[string]Kind, len(tokenList))
	for _, t := range tokenList {
		m[t.Name] = t.Kind
	}
	return m
}()

// Tokens 返回可改写令牌的白名单（按固定顺序），前端导出模板时用它取值。
func Tokens() []Token { return append([]Token(nil), tokenList...) }

// Manifest 是主题文件的内容，也是导出时写出的形状。
type Manifest struct {
	Format      int               `json:"agentbox_theme"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Base        string            `json:"base"`
	Author      string            `json:"author,omitempty"`
	Description string            `json:"description,omitempty"`
	Common      map[string]string `json:"common,omitempty"`
	Dark        map[string]string `json:"dark,omitempty"`
	Light       map[string]string `json:"light,omitempty"`
}

// ValidationError 带一个稳定的 Code，前端按它翻译；Token/Section 只在相关时填写。
type ValidationError struct {
	Code    string `json:"code"`
	Token   string `json:"token,omitempty"`
	Section string `json:"section,omitempty"`
	msg     string
}

func (e *ValidationError) Error() string { return e.msg }

func invalid(code, msg string) *ValidationError { return &ValidationError{Code: code, msg: msg} }

// TooLarge 是请求体超过 MaxBytes 时的错误。
func TooLarge() *ValidationError {
	return invalid("too_large", fmt.Sprintf("主题文件超过 %d KiB", MaxBytes>>10))
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ValidID 判断主题 ID：小写字母、数字与短横线，最多 40 个字符。
func ValidID(id string) bool { return idRE.MatchString(id) }

// Parse 严格解码并校验一份清单：未知字段、尾随内容、超长文件都拒绝。
// 返回的清单已规范化（空白折叠、空分组去掉），可以直接保存。
func Parse(raw []byte) (Manifest, error) {
	var m Manifest
	if len(raw) > MaxBytes {
		return m, TooLarge()
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		if s := err.Error(); strings.HasPrefix(s, "json: unknown field ") {
			unknown := strings.Trim(strings.TrimPrefix(s, "json: unknown field "), `"`)
			return m, &ValidationError{Code: "unknown_field", Token: unknown, msg: "主题文件含不认识的字段 " + unknown}
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return m, &ValidationError{Code: "bad_type", Token: typeErr.Field, msg: "主题文件里 " + typeErr.Field + " 的类型不对"}
		}
		return m, invalid("bad_json", "主题文件不是有效的 JSON 对象")
	}
	if _, err := dec.Token(); err != io.EOF {
		return m, invalid("bad_json", "主题文件在 JSON 对象之后还有多余内容")
	}
	if err := m.Normalize(); err != nil {
		return m, err
	}
	return m, nil
}

// Normalize 校验并就地规范化清单。读回已保存的主题时也走它，磁盘上被改坏的条目不会下发给浏览器。
func (m *Manifest) Normalize() error {
	if m.Format != FormatVersion {
		return invalid("format", fmt.Sprintf("不支持的主题格式版本（需要 agentbox_theme: %d）", FormatVersion))
	}
	if !ValidID(m.ID) {
		return invalid("id", "主题 ID 只能包含小写字母、数字和短横线，最多 40 个字符，且以字母或数字开头")
	}
	m.Name = strings.TrimSpace(m.Name)
	if !plainText(m.Name, maxNameRunes) || m.Name == "" {
		return invalid("name", fmt.Sprintf("主题名称不能为空，最多 %d 个字符，且不能含控制字符", maxNameRunes))
	}
	if !isBase(m.Base) {
		return invalid("base", "base 必须是内置风格之一："+strings.Join(Bases, " / "))
	}
	m.Author = strings.TrimSpace(m.Author)
	if !plainText(m.Author, maxAuthor) {
		return invalid("author", fmt.Sprintf("作者最多 %d 个字符，且不能含控制字符", maxAuthor))
	}
	m.Description = strings.TrimSpace(m.Description)
	if !plainText(m.Description, maxDesc) {
		return invalid("description", fmt.Sprintf("说明最多 %d 个字符，且不能含控制字符", maxDesc))
	}
	for _, section := range []struct {
		name string
		set  *map[string]string
	}{{"common", &m.Common}, {"dark", &m.Dark}, {"light", &m.Light}} {
		if len(*section.set) == 0 {
			*section.set = nil
			continue
		}
		out := make(map[string]string, len(*section.set))
		for name, value := range *section.set {
			kind, ok := tokenKinds[name]
			if !ok {
				return &ValidationError{Code: "unknown_token", Token: name, Section: section.name,
					msg: fmt.Sprintf("%s 里的 %s 不是可改写的主题令牌", section.name, name)}
			}
			v, err := checkValue(kind, value)
			if err != nil {
				return &ValidationError{Code: "bad_value", Token: name, Section: section.name,
					msg: fmt.Sprintf("%s 里 %s 的取值无效：%s", section.name, name, err)}
			}
			out[name] = v
		}
		*section.set = out
	}
	return nil
}

func isBase(s string) bool {
	for _, b := range Bases {
		if b == s {
			return true
		}
	}
	return false
}

func plainText(s string, max int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

var (
	colorFuncs = funcSet("rgb", "rgba", "hsl", "hsla", "hwb", "lab", "lch", "oklab", "oklch", "color", "color-mix")
	canvasFunc = funcSet("rgb", "rgba", "hsl", "hsla", "hwb", "lab", "lch", "oklab", "oklch", "color", "color-mix",
		"linear-gradient", "radial-gradient", "conic-gradient",
		"repeating-linear-gradient", "repeating-radial-gradient", "repeating-conic-gradient")
	numberRE = regexp.MustCompile(`^(?:\d+(?:\.\d+)?|\.\d+)$`)
	radiusRE = regexp.MustCompile(`^(?:\d+(?:\.\d+)?|\.\d+)(?:px|%)$`)
	identRE  = regexp.MustCompile(`^-?[A-Za-z_][A-Za-z0-9_-]*$`)
	hexRE    = regexp.MustCompile(`^#(?:[0-9A-Fa-f]{3,4}|[0-9A-Fa-f]{6}|[0-9A-Fa-f]{8})$`)
)

func funcSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// checkValue 校验一个取值并返回规范化后的形式（连续空白折成一个空格）。
func checkValue(kind Kind, value string) (string, error) {
	v := strings.Join(strings.Fields(value), " ")
	if v == "" {
		return "", errors.New("不能为空")
	}
	switch kind {
	case KindScale:
		f, err := strconv.ParseFloat(v, 64)
		if !numberRE.MatchString(v) || err != nil || f > 3 {
			return "", errors.New("需要 0 到 3 之间的数字")
		}
		return v, nil
	case KindRadius:
		f, _ := strconv.ParseFloat(strings.TrimRight(v, "px%"), 64)
		if !radiusRE.MatchString(v) || f > 9999 {
			return "", errors.New("需要 px 或 % 长度，例如 999px、50%")
		}
		return v, nil
	case KindFont:
		return checkFont(v)
	case KindCanvas:
		return v, checkSafe(v, canvasFunc, maxCanvas)
	default: // color / shadow
		return v, checkSafe(v, colorFuncs, maxValue)
	}
}

// checkSafe 是颜色、投影、渐变共用的「安全取值」语法：
//   - 字符只允许字母、数字、空格与 # % . , ( ) / + -；没有引号、反斜杠（CSS 转义能拼出
//     u\72l( 绕过名字检查）、分号、花括号、冒号、感叹号、星号（注释）；
//   - 每个左括号前必须紧挨一个白名单里的函数名；url()、image-set()、element() 等不在内；
//   - var() 只能引用白名单令牌，且不带回退值；
//   - # 后只能是 3/4/6/8 位十六进制色值。
func checkSafe(v string, funcs map[string]bool, max int) error {
	if len(v) > max {
		return fmt.Errorf("超过 %d 个字符", max)
	}
	for _, r := range v {
		if r > unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(" #%.,()/+-", r)) {
			return fmt.Errorf("含不允许的字符 %q", r)
		}
	}
	depth := 0
	for i := 0; i < len(v); {
		c := v[i]
		switch {
		case c == '(':
			return errors.New("括号前缺少函数名")
		case c == ')':
			depth--
			if depth < 0 {
				return errors.New("括号不配对")
			}
			i++
		case c == '#':
			j := i + 1
			for j < len(v) && isWordByte(v[j]) {
				j++
			}
			if !hexRE.MatchString(v[i:j]) {
				return fmt.Errorf("%s 不是十六进制颜色", v[i:j])
			}
			i = j
		case isWordByte(c):
			j := i
			for j < len(v) && isWordByte(v[j]) {
				j++
			}
			word := v[i:j]
			if j < len(v) && v[j] == '(' {
				name := strings.ToLower(word)
				if name == "var" {
					end := strings.IndexByte(v[j:], ')')
					if end < 0 {
						return errors.New("var() 未闭合")
					}
					ref := strings.TrimSpace(v[j+1 : j+end])
					if _, ok := tokenKinds[ref]; !ok {
						return fmt.Errorf("var() 只能引用主题令牌，不能引用 %q", ref)
					}
					i = j + end + 1
					continue
				}
				if !funcs[name] {
					return fmt.Errorf("不允许的函数 %s()", word)
				}
				depth++
				if depth > maxParenDepth {
					return errors.New("函数嵌套过深")
				}
				i = j + 1
				continue
			}
			i = j
		default: // 空格 , / % . + 等分隔与数字符号
			i++
		}
	}
	if depth != 0 {
		return errors.New("括号不配对")
	}
	return nil
}

func isWordByte(c byte) bool {
	return c == '-' || c == '_' || c == '.' || c == '+' || c == '%' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// checkFont 校验 font-family 列表：每项是带双引号的家族名（字母、数字、空格、. _ -，
// 可以是中文），或不带引号的标识符（sans-serif、ui-monospace、SFMono-Regular）。
// 只能用本机已装的字体：不开放 @font-face，也就没有网络字体。
func checkFont(v string) (string, error) {
	if len(v) > maxFont {
		return "", fmt.Errorf("超过 %d 个字符", maxFont)
	}
	parts := strings.Split(v, ",")
	if len(parts) > maxFontFamily {
		return "", fmt.Errorf("最多 %d 个字体", maxFontFamily)
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
			name := p[1 : len(p)-1]
			if name == "" {
				return "", errors.New("字体名为空")
			}
			for _, r := range name {
				if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '.' || r == '_' || r == '-') {
					return "", fmt.Errorf("字体名 %q 含不允许的字符", name)
				}
			}
		} else {
			for _, w := range strings.Fields(p) {
				if !identRE.MatchString(w) {
					return "", fmt.Errorf("%q 不是字体名；含空格或非英文字符的字体名请加双引号", p)
				}
			}
			if p == "" {
				return "", errors.New("字体列表里有空项")
			}
		}
		out = append(out, p)
	}
	return strings.Join(out, ", "), nil
}
