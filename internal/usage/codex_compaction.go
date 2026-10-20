package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"regexp"
	"time"

	"agentbox/internal/store"
)

// Codex 压缩上下文也是一次模型请求，0.16x 压缩完在会话记录里写一条 compacted，
// latest_token_usage_record 就是那次请求的用量（带回合 id）。它进不进 tokenUsage
// / token_count 的累计取决于压缩方式（0.163 实测）：
//
//   - API Key / 中转的本地压缩是一次普通的 /responses 请求，照常进累计，紧挨在
//     compacted 前面的那次请求就是它，已经记过；
//   - ChatGPT 登录的远程压缩不进累计（之后的 token_count 累计不变、last 为 0），
//     只能从 compacted 补一行，否则整次压缩都漏掉——长对话里这一次常是几十万
//     token 的输入。
//
// 所以 compacted 的用量和本回合已记下的某次请求完全相同时不再记。网页对话在
// 结算时按回合 id 读这条线程的会话记录（app-server 的收尾事件带着压缩次数，没压
// 缩过的回合不读）；终端补记在 codex_terminal.go 里顺着记录读到。

type codexCompaction struct {
	TurnID string
	TS     time.Time
	Usage  codexTokens
}

// codexCompactionRecord 是 compacted 记录里用得上的部分。
type codexCompactionRecord struct {
	Record *struct {
		TurnID     string       `json:"turn_id"`
		ResponseID string       `json:"response_id"`
		Usage      *codexTokens `json:"usage"`
	} `json:"latest_token_usage_record"`
}

func (r codexCompactionRecord) usage() (turn, response string, u codexTokens, ok bool) {
	if r.Record == nil || r.Record.Usage == nil {
		return "", "", codexTokens{}, false
	}
	u = *r.Record.Usage
	if u.Input < 0 || u.Cached < 0 || u.Output < 0 || u.CacheWrite < 0 || u.Input == 0 && u.Output == 0 {
		return "", "", codexTokens{}, false
	}
	return r.Record.TurnID, r.Record.ResponseID, u, true
}

// sameRequest 比较两份单次请求的用量（输入含缓存命中）。
func sameRequest(a, b codexTokens) bool {
	return a.Input == b.Input && a.Cached == b.Cached && a.Output == b.Output && a.CacheWrite == b.CacheWrite
}

var codexThreadIDRE = regexp.MustCompile(`^[0-9A-Za-z-]{8,64}$`)

// compactionScan 收集一份会话记录里属于某个回合的 compacted。
type compactionScan struct {
	turn string
	out  []codexCompaction
}

var compactedMarker = []byte(`"compacted"`)

func (c *compactionScan) line(line []byte) {
	if !bytes.Contains(line, compactedMarker) {
		return // 绝大多数行不是：不解码
	}
	var rec struct {
		Type      string                `json:"type"`
		Timestamp string                `json:"timestamp"`
		Payload   codexCompactionRecord `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "compacted" {
		return
	}
	turn, _, u, ok := rec.Payload.usage()
	if !ok || turn != c.turn {
		return
	}
	ts, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
	c.out = append(c.out, codexCompaction{TurnID: turn, TS: ts, Usage: u})
}

func (c *compactionScan) rows() []termTurn { return nil }

// codexCompactions 读会话 home 里这条线程的会话记录，返回该回合的压缩请求用量。
func (s *Service) codexCompactions(home, thread, turn string) ([]codexCompaction, error) {
	if !codexThreadIDRE.MatchString(thread) || turn == "" {
		return nil, fmt.Errorf("invalid thread %q", thread)
	}
	root, err := s.openDataDir(filepath.Join(home, ".codex", "sessions"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	files, err := fs.Glob(root.FS(), "*/*/*/rollout-*-"+thread+".jsonl")
	if err != nil {
		return nil, err
	}
	scan := &compactionScan{turn: turn}
	for _, path := range files {
		f, err := root.OpenFile(path)
		if err != nil {
			return nil, err
		}
		err = feedTranscript(f, scan)
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	return scan.out, nil
}

// CodexRolloutExists reports whether a Codex conversation keeps a session
// record (rollout) in the session's home. Telemetry turns a request into a
// background row only when it does not: a conversation with a rollout is
// accounted from it.
func (s *Service) CodexRolloutExists(sess store.Session, conversation string) (bool, error) {
	if !codexThreadIDRE.MatchString(conversation) {
		return false, fmt.Errorf("invalid conversation %q", conversation)
	}
	root, err := s.openDataDir(filepath.Join(s.homeDir(sess), ".codex", "sessions"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer root.Close()
	files, err := fs.Glob(root.FS(), "*/*/*/rollout-*-"+conversation+".jsonl")
	return len(files) > 0, err
}

// codexTurnEnd 是网页对话里压缩过的回合：结算时去会话记录里补压缩请求。
type codexTurnEnd struct {
	base         store.UsageEvent
	thread, turn string
}

// observeCodexTurnEnd 认出带压缩次数的 app-server 收尾事件。用量全零（压缩完就
// 被中断）的回合 ParseUsage 不出行，这里照样要记下。
func (t *Tally) observeCodexTurnEnd(base store.UsageEvent, line []byte) {
	if !bytes.Contains(line, []byte(`"compactions"`)) {
		return
	}
	var ev struct {
		Type        string `json:"type"`
		ThreadID    string `json:"thread_id"`
		TurnID      string `json:"turn_id"`
		Compactions int    `json:"compactions"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Compactions <= 0 || ev.ThreadID == "" || ev.TurnID == "" ||
		ev.Type != "turn.completed" && ev.Type != "turn.failed" {
		return
	}
	t.codexTurns = append(t.codexTurns, codexTurnEnd{base: base, thread: ev.ThreadID, turn: ev.TurnID})
}

// compactionRows 把网页回合里没进 tokenUsage 的压缩请求补成行（与逐次请求行同
// 口径：一次请求一行，按对照键等遥测计时）。recorded 是本次结算已有的行。
func (s *Service) compactionRows(t *Tally, recorded []store.UsageEvent) []store.UsageEvent {
	var out []store.UsageEvent
	for _, end := range t.codexTurns {
		found, err := s.codexCompactions(t.codexHome, end.thread, end.turn)
		if err != nil {
			log.Printf("usage: Codex 压缩记录 %s: %v", end.base.SessionID, err)
			continue
		}
		for _, c := range found {
			counted := false
			for _, r := range recorded {
				if r.SessionID == end.base.SessionID && r.TurnID == end.base.TurnID &&
					sameRequest(codexTokens{Input: r.InputTokens + r.CacheReadTokens, Cached: r.CacheReadTokens, Output: r.OutputTokens, CacheWrite: r.CacheWriteTokens}, c.Usage) {
					counted = true
					break
				}
			}
			if counted {
				continue
			}
			row := end.base
			row.InputTokens = max(c.Usage.Input-c.Usage.Cached, 0)
			row.CacheReadTokens = c.Usage.Cached
			row.CacheWriteTokens = c.Usage.CacheWrite
			row.OutputTokens = c.Usage.Output
			row.TTFTMs, row.DurationMS, row.WallMS = 0, 0, 0
			if !c.TS.IsZero() {
				row.TS = c.TS
			}
			raw, _ := json.Marshal(struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
				codexTokens
			}{"codex.compaction", c.TurnID, c.Usage})
			row.Raw = string(raw)
			row.APIRequestID = CodexRequestKey(end.thread, c.Usage.Input, c.Usage.Cached, c.Usage.Output)
			out = append(out, row)
		}
	}
	return out
}
