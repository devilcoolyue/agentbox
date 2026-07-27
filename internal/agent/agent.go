// Package agent adapts the two supported CLI coding agents (Claude Code and
// Codex CLI) to a common shape: how to launch a headless chat turn, how to
// launch an interactive terminal, and how to seed account credentials into a
// session home directory.
package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"agentbox/internal/config"
)

// PidFile is written inside the container by every headless chat turn so the
// server can deliver SIGINT for user-initiated interrupts.
const PidFile = "/tmp/.agentbox-chat.pid"

var modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// claudeThinking maps the abstract effort level chosen in the UI to a
// MAX_THINKING_TOKENS budget; Claude Code has no CLI flag for this.
var claudeThinking = map[string]string{
	"low": "4096", "medium": "13000", "high": "24000", "xhigh": "31999",
}

var codexEfforts = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true,
}

// ChatCommand returns the argv for one headless conversation turn. The prompt
// is delivered on stdin, which avoids any shell-quoting of user text. The
// command is wrapped in `sh -c` solely to record the PID for interrupts.
// model and effort are optional per-turn overrides ("" keeps the account /
// CLI default); effort is an abstract level (low|medium|high|xhigh).
func ChatCommand(agentType, permissionMode, resumeID, model, effort string) ([]string, error) {
	if model != "" && !modelRe.MatchString(model) {
		return nil, fmt.Errorf("模型名 %q 无效", model)
	}
	prelude := ""
	var args []string
	switch agentType {
	case config.AgentClaude:
		args = []string{
			"claude", "-p",
			"--output-format", "stream-json",
			"--include-partial-messages",
			"--verbose",
			"--permission-mode", permissionMode,
		}
		if model != "" {
			args = append(args, "--model", model)
		}
		if effort != "" {
			tokens, ok := claudeThinking[effort]
			if !ok {
				return nil, fmt.Errorf("思考强度 %q 无效", effort)
			}
			prelude = "export MAX_THINKING_TOKENS=" + tokens + "; "
		}
		if resumeID != "" {
			args = append(args, "--resume", resumeID)
		}
	case config.AgentCodex:
		// The container itself is the sandbox, so codex runs with full access.
		base := []string{"codex", "exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox"}
		if model != "" {
			base = append(base, "-m", model)
		}
		if effort != "" {
			if !codexEfforts[effort] {
				return nil, fmt.Errorf("思考强度 %q 无效", effort)
			}
			base = append(base, "-c", `model_reasoning_effort="`+effort+`"`)
		}
		if resumeID != "" {
			args = append(base[:2:2], "resume", resumeID)
			args = append(args, base[2:]...)
		} else {
			args = base
		}
		args = append(args, "-") // read prompt from stdin
	default:
		return nil, fmt.Errorf("unknown agent type %q", agentType)
	}
	wrapped := append([]string{"/bin/sh", "-c", `echo $$ >` + PidFile + `; ` + prelude + `exec "$@"`, "agentbox"}, args...)
	return wrapped, nil
}

// TitleCommand returns the argv for a one-shot, tool-free summarization that
// names a conversation thread. The prompt (instruction + opening message) is
// delivered on stdin; the command prints ONLY the resulting title to stdout.
// It never resumes and never records a provider session, so it can't disturb
// the real conversation. Tools are disabled and a small/fast model is used to
// keep it cheap and inert.
func TitleCommand(agentType string) ([]string, error) {
	switch agentType {
	case config.AgentClaude:
		// --tools "" 彻底禁用工具；haiku 足够快且便宜。用 json 而非 text 输出：
		// 同一个对象里既有标题（result 字段）又有 token/费用，起标题这点消耗
		// 才能计入用量流水（见 TitleOutput）。
		return []string{"claude", "-p", "--model", "haiku", "--tools", "", "--output-format", "json"}, nil
	case config.AgentCodex:
		// codex exec 没有“禁用全部工具”的开关，但纯总结提示不会触发命令；
		// 起标题不需要推理深度，用 low 思考强度压低成本与时延。单引号保住
		// TOML 值里的双引号，sh 才会把 model_reasoning_effort="low" 原样传给
		// codex。最终消息写入临时文件后单独 cat，避开 stdout 上的框架噪声。
		//
		// --json 把事件流引到另一个文件，回合末尾的 turn.completed 带着 token
		// 用量：标题之后跟一行分隔符再跟这条事件，由 TitleOutput 拆开，起标题
		// 的消耗才不会漏账。
		const in, out, ev = "/tmp/.abox-title.in", "/tmp/.abox-title.out", "/tmp/.abox-title.jsonl"
		script := "cat >" + in + "; " +
			"codex exec --json --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox " +
			"-c 'model_reasoning_effort=\"low\"' " +
			"--output-last-message " + out + " - <" + in + " >" + ev + " 2>/dev/null; " +
			"cat " + out + " 2>/dev/null; " +
			"printf '\\n%s\\n' " + titleUsageMarker + "; " +
			"grep -F '\"turn.completed\"' " + ev + " 2>/dev/null | tail -1; " +
			"rm -f " + in + " " + out + " " + ev
		return []string{"/bin/sh", "-c", script}, nil
	default:
		return nil, fmt.Errorf("unknown agent type %q", agentType)
	}
}

// titleUsageMarker separates the title from the usage event on Codex's title
// stdout. Quoted as a shell literal where TitleCommand builds the script.
const titleUsageMarker = "'---abox-usage---'"

// TitleOutput splits what TitleCommand wrote on stdout into the title text and,
// when the agent reported it, the raw usage-bearing event.
//
// Claude answers with a single `--output-format json` object: the title sits in
// `result`, and the very same object carries usage/modelUsage/total_cost_usd,
// so it can be handed straight to the usage parser.
//
// Codex prints the title, then a marker line, then its turn.completed event.
// The marker is matched from the end so a title that happens to contain it
// cannot swallow the usage line.
//
// Either agent falling back to plain text (an older CLI ignoring the flags, a
// killed process truncating the JSON) still yields a usable title, just no
// usage — better a title without accounting than neither.
func TitleOutput(agentType, out string) (title string, usage []byte) {
	switch agentType {
	case config.AgentClaude:
		trimmed := strings.TrimSpace(out)
		var res struct {
			Type   string `json:"type"`
			Result string `json:"result"`
		}
		if json.Unmarshal([]byte(trimmed), &res) != nil || res.Type != "result" {
			return out, nil
		}
		return res.Result, []byte(trimmed)

	case config.AgentCodex:
		marker := strings.Trim(titleUsageMarker, "'")
		i := strings.LastIndex(out, marker)
		if i < 0 {
			return out, nil
		}
		title, rest := out[:i], strings.TrimSpace(out[i+len(marker):])
		if rest == "" || !json.Valid([]byte(rest)) {
			return title, nil
		}
		return title, []byte(rest)
	}
	return out, nil
}

// InterruptCommand kills the current chat turn, if any.
func InterruptCommand() []string {
	return []string{"/bin/sh", "-c", "kill -INT $(cat " + PidFile + " 2>/dev/null) 2>/dev/null || true"}
}

// claudeSeedState pre-accepts first-run dialogs so both headless and
// interactive modes work immediately in a fresh session home.
var claudeSeedState = map[string]any{
	"hasCompletedOnboarding":        true,
	"bypassPermissionsModeAccepted": true,
	"projects": map[string]any{
		"/workspace": map[string]any{
			"hasTrustDialogAccepted":        true,
			"hasCompletedProjectOnboarding": true,
		},
	},
}

// RotatingCredFile returns the OAuth credential file that the agent CLI
// rewrites on token refresh, as (file name inside the account pool dir,
// path relative to the session home). Refresh tokens are rotated on use, so
// the pool copy and every session copy must stay on one token chain — the
// server's credSync keeps them converged.
func RotatingCredFile(agentType string) (poolName, homeRel string) {
	switch agentType {
	case config.AgentCodex:
		return "auth.json", ".codex/auth.json"
	default:
		return ".credentials.json", ".claude/.credentials.json"
	}
}

// SeedCredentials refreshes account credentials inside the session home
// directory. It is called on every session start so re-logins on the pool
// account propagate to existing sessions.
func SeedCredentials(agentType, homeDir, credDir string, uid, gid int) error {
	var target string
	switch agentType {
	case config.AgentClaude:
		target = filepath.Join(homeDir, ".claude")
	case config.AgentCodex:
		target = filepath.Join(homeDir, ".codex")
	default:
		return fmt.Errorf("unknown agent type %q", agentType)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	if err := os.Chown(target, uid, gid); err != nil {
		return err
	}

	if credDir != "" {
		entries, err := os.ReadDir(credDir)
		if err != nil {
			return fmt.Errorf("account credentials dir: %w", err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			if err := copyFile(filepath.Join(credDir, e.Name()), filepath.Join(target, e.Name()), uid, gid); err != nil {
				return err
			}
		}
	}

	if agentType == config.AgentClaude {
		stateFile := filepath.Join(homeDir, ".claude.json")
		if _, err := os.Stat(stateFile); os.IsNotExist(err) {
			raw, err := json.MarshalIndent(claudeSeedState, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
				return err
			}
			if err := os.Chown(stateFile, uid, gid); err != nil {
				return err
			}
		}
		if err := seedClaudeHUD(homeDir, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// intranetHintMarkers delimit the managed block so re-seeding on every start
// replaces it in place instead of appending duplicates.
const (
	intranetHintBegin = "<!-- agentbox:intranet-proxy (managed, do not edit) -->"
	intranetHintEnd   = "<!-- /agentbox:intranet-proxy -->"
)

// intranetHintBody tells the agent how to reach the user's LAN/intranet through
// the reverse tunnel. It keys off $AGENTBOX_INTRANET_PROXY, which is present in
// the exec env only while the user's tunnel is live — so the guidance is inert
// when no tunnel is up.
const intranetHintBody = `## 访问用户内网 / 局域网（agentbox 内网隧道）

当环境变量 ` + "`$AGENTBOX_INTRANET_PROXY`" + ` 存在时，表示用户已开启内网反向隧道。
需要访问只有用户本机 / 内网才能连通的地址（内网 IP、内网域名、局域网服务）时，
经该 SOCKS5 代理发起请求，例如：

    curl --proxy "$AGENTBOX_INTRANET_PROXY" http://gitlab.corp.local/...
    git -c http.proxy="$AGENTBOX_INTRANET_PROXY" clone http://10.0.0.5/repo.git

若还存在 ` + "`$AGENTBOX_INTRANET_MAPS`" + `（逗号分隔的 ` + "`监听地址=内网目标`" + ` 列表，如
` + "`172.17.0.1:3306=10.0.1.5:3306`" + `），则每个监听地址是对应内网目标的直连 TCP
端口——psql / mysql / redis-cli 及各类数据库驱动等不支持 SOCKS 的程序直接连
监听地址即可，例如：

    mysql -h 172.17.0.1 -P 3306 ...   # 实际连到内网 10.0.1.5:3306

注意：代理与映射端口仅用于内网目标；公网与模型 API 请求请直连，不要走此代理。
若上述变量不存在，则当前无内网隧道可用，勿尝试代理。`

// SeedIntranetHint writes/refreshes the intranet-proxy guidance into the agent's
// home-level instructions file (claude: ~/.claude/CLAUDE.md, codex:
// ~/.codex/AGENTS.md) so the agent knows to use $AGENTBOX_INTRANET_PROXY. The
// managed block is delimited by markers and replaced in place; any surrounding
// user content is preserved.
func SeedIntranetHint(agentType, homeDir string, uid, gid int) error {
	var path string
	switch agentType {
	case config.AgentClaude:
		path = filepath.Join(homeDir, ".claude", "CLAUDE.md")
	case config.AgentCodex:
		path = filepath.Join(homeDir, ".codex", "AGENTS.md")
	default:
		return fmt.Errorf("unknown agent type %q", agentType)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	block := intranetHintBegin + "\n" + intranetHintBody + "\n" + intranetHintEnd

	existing, _ := os.ReadFile(path)
	var next string
	if s := string(existing); strings.Contains(s, intranetHintBegin) && strings.Contains(s, intranetHintEnd) {
		pre := s[:strings.Index(s, intranetHintBegin)]
		post := s[strings.Index(s, intranetHintEnd)+len(intranetHintEnd):]
		next = pre + block + post
	} else if len(existing) == 0 {
		next = block + "\n"
	} else {
		next = strings.TrimRight(s, "\n") + "\n\n" + block + "\n"
	}
	if next == string(existing) {
		return nil
	}
	return writeOwned(path, []byte(next), uid, gid)
}

// hudStatusLineCmd 驱动镜像内置的 claude-hud（/opt/claude-hud）。终端宽度
// 优先取 COLUMNS，取不到再问 /dev/tty，最后兜底 120。
const hudStatusLineCmd = `bash -c 'cols=${COLUMNS:-}; case "$cols" in ""|*[!0-9]*) cols=$(stty size </dev/tty 2>/dev/null | cut -d" " -f2);; esac; case "$cols" in ""|*[!0-9]*) cols=120;; esac; export COLUMNS=$((cols>4?cols-4:1)); exec node /opt/claude-hud/dist/index.js'`

// seedClaudeHUD 给会话 home 种上 claude-hud 状态栏：settings.json 指向镜像
// 内置的 dist，HUD 显示配置与宿主机保持一致。两个文件都只在缺失时写入，
// 不覆盖用户在容器里的后续修改。
func seedClaudeHUD(homeDir string, uid, gid int) error {
	// settings.json 可能已被容器里的 claude 自己写过（如权限提示的记忆），
	// 所以是合并而不是缺失才写：只在没有 statusLine 键时补上，其余原样保留。
	settings := filepath.Join(homeDir, ".claude", "settings.json")
	cur := map[string]any{}
	if raw, err := os.ReadFile(settings); err == nil {
		if json.Unmarshal(raw, &cur) != nil {
			cur = nil // 解析不了的用户文件不碰
		}
	}
	if cur != nil {
		if _, has := cur["statusLine"]; !has {
			cur["statusLine"] = map[string]any{
				"type":    "command",
				"command": hudStatusLineCmd,
			}
			raw, _ := json.MarshalIndent(cur, "", "  ")
			if err := writeOwned(settings, raw, uid, gid); err != nil {
				return err
			}
		}
	}

	hudDir := filepath.Join(homeDir, ".claude", "plugins", "claude-hud")
	for _, d := range []string{filepath.Dir(hudDir), hudDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		if err := os.Chown(d, uid, gid); err != nil {
			return err
		}
	}
	cfg := filepath.Join(hudDir, "config.json")
	if _, err := os.Stat(cfg); os.IsNotExist(err) {
		raw, _ := json.MarshalIndent(map[string]any{
			"display": map[string]any{
				"showTools":         true,
				"showAgents":        true,
				"showTodos":         true,
				"sevenDayThreshold": 0,
			},
		}, "", "  ")
		if err := writeOwned(cfg, raw, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

func writeOwned(path string, data []byte, uid, gid int) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

func copyFile(src, dst string, uid, gid int) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chown(dst, uid, gid)
}

// IsPartialEvent reports whether a stream line is an incremental
// --include-partial-messages event (type "stream_event"). These carry
// content-block deltas for live rendering; the complete assistant event that
// follows repeats the full content, so partials are broadcast but never
// persisted.
func IsPartialEvent(line []byte) bool {
	var ev struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return false
	}
	return ev.Type == "stream_event"
}

// ExtractSessionID pulls the provider conversation id out of a stream event,
// if present. Claude Code emits session_id on init/result events; Codex emits
// thread/session ids depending on version, so several keys are probed.
func ExtractSessionID(line []byte) string {
	var ev map[string]any
	if err := json.Unmarshal(line, &ev); err != nil {
		return ""
	}
	if id := stringField(ev, "session_id"); id != "" {
		return id
	}
	if id := stringField(ev, "thread_id"); id != "" {
		return id
	}
	if msg, ok := ev["msg"].(map[string]any); ok {
		if id := stringField(msg, "session_id"); id != "" {
			return id
		}
		if id := stringField(msg, "thread_id"); id != "" {
			return id
		}
	}
	return ""
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
