package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"agentbox/internal/store"
)

// termRowsSnapshot 是终端行里与解析相关的部分，按 req_id 排好，便于比较两种读法。
func termRowsSnapshot(st *store.Store) []string {
	var out []string
	for _, r := range st.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal}) {
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%d/%d/%d/%d|%s", r.ReqID, r.TurnID, r.Model, r.TS.UTC().Format("15:04:05.000"),
			r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens, r.Effort))
	}
	sort.Strings(out)
	return out
}

func putTermSession(t *testing.T, s *fixtureService, sess store.Session, agentName string) store.Session {
	t.Helper()
	sess.Agent = agentName
	if err := s.store.Put(sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

// 续读必须与整份重读的结果完全一致：把同一份 transcript 按各种切法（包括切在
// 一行中间）一段段追加，每追加一段扫一次，最终流水与一次读完的完全相同。
func TestTerminalIncrementalMatchesFullParse(t *testing.T) {
	claude := strings.ReplaceAll(termFixture, `"type":"assistant",`, `"type":"assistant","effort":"high",`)
	// 同一条消息先写占位、后写最终值：续读时最终值落在后一段里。
	claude += `{"type":"user","entrypoint":"cli","uuid":"u3","timestamp":"2026-08-11T10:03:00.000Z","message":{"role":"user"}}
{"type":"assistant","entrypoint":"cli","requestId":"req_E","timestamp":"2026-08-11T10:03:01.000Z","message":{"id":"msg_5","model":"claude-sonnet-5","usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":10}}}
{"type":"assistant","entrypoint":"cli","requestId":"req_E","timestamp":"2026-08-11T10:03:04.000Z","message":{"id":"msg_5","model":"claude-sonnet-5","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":80,"cache_read_input_tokens":10}}}
`
	cases := []struct {
		agent, rel, raw string
	}{
		{"claude", filepath.Join(".claude", "projects", "-workspace", "abc.jsonl"), claude},
		{"codex", filepath.Join(".codex", "sessions", "2026", "09", "23", "rollout-fixture.jsonl"), codexFixture(t)},
	}
	for _, c := range cases {
		t.Run(c.agent, func(t *testing.T) {
			full, sess := newTestServer(t)
			sess = putTermSession(t, full, sess, c.agent)
			path := filepath.Join(full.homeDir(sess), c.rel)
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, []byte(c.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			full.Scan()
			want := termRowsSnapshot(full.store)
			if len(want) == 0 {
				t.Fatal("fixture produced no rows")
			}

			for _, step := range []int{1, 7, 64, 333} {
				inc, sess := newTestServer(t)
				sess = putTermSession(t, inc, sess, c.agent)
				path := filepath.Join(inc.homeDir(sess), c.rel)
				os.MkdirAll(filepath.Dir(path), 0o755)
				f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < len(c.raw); i += step {
					if _, err := f.WriteString(c.raw[i:min(i+step, len(c.raw))]); err != nil {
						t.Fatal(err)
					}
					// 每段都扫：inotify 在一行写到一半时也可能触发。
					if i%(step*5) == 0 {
						inc.scanTerminalUsage(inc.termScan)
					}
				}
				f.Close()
				inc.scanTerminalUsage(inc.termScan)
				if got := termRowsSnapshot(inc.store); strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Fatalf("step %d:\n got %v\nwant %v", step, got, want)
				}
			}
		})
	}
}

// 文件被重写过，不能接着旧位置读下去，要从头再读：原地截短重写（续读点前的
// 尾巴变了），或者写新文件再改名替换（inode 变了，哪怕尾巴恰好相同）。
func TestTerminalRewrittenFileIsReadAgain(t *testing.T) {
	extra := `{"type":"assistant","entrypoint":"cli","requestId":"req_Y","timestamp":"2026-08-11T10:02:09.000Z","message":{"id":"msg_Y","model":"claude-haiku-4-5","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	for name, rewrite := range map[string]func(path, raw string) error{
		"in place": func(path, raw string) error {
			// 改动落在旧续读点之前的最后几十个字节里。
			return os.WriteFile(path, []byte(strings.Replace(raw, `"output_tokens":20,"cache_read_input_tokens":0`, `"output_tokens":20,"cache_read_input_tokens":9`, 1)), 0o600)
		},
		"renamed": func(path, raw string) error {
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, []byte(raw), 0o600); err != nil {
				return err
			}
			return os.Rename(tmp, path)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, sess := newTestServer(t)
			sess = putTermSession(t, s, sess, "claude")
			path := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace", "abc.jsonl")
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, []byte(termFixture), 0o600); err != nil {
				t.Fatal(err)
			}
			s.Scan()
			// 同长度地换掉前面的 ID：只有从头读才看得到 req_X。
			raw := strings.ReplaceAll(termFixture, "req_A", "req_X")
			raw = strings.ReplaceAll(raw, "msg_1", "msg_X") + extra
			if err := rewrite(path, raw); err != nil {
				t.Fatal(err)
			}
			s.Scan()
			got := strings.Join(termRowsSnapshot(s.store), "\n")
			if !strings.Contains(got, "req_X|req_X|") || !strings.Contains(got, "req_Y|req_D|") {
				t.Fatalf("rewritten transcript was continued from a stale offset:\n%s", got)
			}
		})
	}
}

// 只有内容变了的行才写库并通知；没有新用量的写入（用户消息、工具结果）不该
// 惊动使用记录页。
func TestTerminalScanWritesAndNotifiesOnlyChanges(t *testing.T) {
	s, sess := newTestServer(t)
	sess = putTermSession(t, s, sess, "claude")
	var notices [][]store.UsageWrite
	s.store.OnUsageWritten(func(w []store.UsageWrite) { notices = append(notices, w) })
	path := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace", "abc.jsonl")
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(termFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Scan()
	if len(notices) != 1 || len(notices[0]) != 1 || notices[0][0] != (store.UsageWrite{User: sess.User, SessionID: sess.ID}) {
		t.Fatalf("first scan notices = %+v", notices)
	}
	appendLine := func(line string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	appendLine(`{"type":"user","entrypoint":"cli","uuid":"u7","timestamp":"2026-08-11T10:05:00.000Z","message":{"role":"user"}}` + "\n")
	s.Scan()
	if len(notices) != 1 {
		t.Fatalf("a line without usage caused a write: %+v", notices)
	}
	// 写到一半的行不算；补完换行后才记上。
	half := `{"type":"assistant","entrypoint":"cli","requestId":"req_F","timestamp":"2026-08-11T10:05:02.000Z","message":{"id":"msg_F","model":"claude-sonnet-5","usage":{"input_tokens":5,"output_tokens":9}}}`
	appendLine(half[:40])
	s.Scan()
	if len(notices) != 1 {
		t.Fatalf("a half-written line was read: %+v", notices)
	}
	appendLine(half[40:] + "\n")
	s.Scan()
	if len(notices) != 2 {
		t.Fatalf("completed line was not recorded: %+v", notices)
	}
	rows := s.store.ListUsage(store.UsageFilter{Kind: store.UsageKindTerminal})
	if len(rows) != 4 || rows[0].ReqID != "req_F" || rows[0].TurnID != "req_F" || rows[0].OutputTokens != 9 {
		t.Fatalf("rows after append: %+v", rows)
	}
}

// 删掉的文件在下一趟全量扫描后被忘掉；闲置久了的文件丢掉续读状态，之后再写
// 从头读起，结果不变。
func TestTerminalScannerForgetsAndDropsIdleState(t *testing.T) {
	s, sess := newTestServer(t)
	sess = putTermSession(t, s, sess, "claude")
	dir := filepath.Join(s.homeDir(sess), ".claude", "projects", "-workspace")
	os.MkdirAll(dir, 0o755)
	keep, gone := filepath.Join(dir, "keep.jsonl"), filepath.Join(dir, "gone.jsonl")
	for _, p := range []string{keep, gone} {
		if err := os.WriteFile(p, []byte(termFixture), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s.Scan()
	if len(s.termScan.seen) != 2 {
		t.Fatalf("seen = %d", len(s.termScan.seen))
	}
	os.Remove(gone)
	s.termScan.seen[keep].active = s.termScan.seen[keep].active.Add(-2 * termParseIdle)
	s.Scan()
	st, ok := s.termScan.seen[keep]
	if len(s.termScan.seen) != 1 || !ok || st.parser != nil || st.written != nil {
		t.Fatalf("after sweep: %d entries, keep=%+v", len(s.termScan.seen), st)
	}
	before := termRowsSnapshot(s.store)
	f, _ := os.OpenFile(keep, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"type":"user","entrypoint":"cli","uuid":"u8","timestamp":"2026-08-11T10:06:00.000Z","message":{"role":"user"}}` + "\n")
	f.Close()
	s.Scan()
	if after := termRowsSnapshot(s.store); strings.Join(after, "\n") != strings.Join(before, "\n") || s.termScan.seen[keep].parser == nil {
		t.Fatalf("re-read after idle changed rows:\n%v\n%v", before, after)
	}
}
