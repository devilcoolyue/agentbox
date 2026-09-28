package usage

// 终端消耗补记：把用户在「终端」页签里手敲 claude 花掉的量记进 usage_events。
//
// 这条路径不经过服务端——终端里的 CLI 是容器内进程，输出直接进 PTY，`runTurn`
// 一个字节都看不到。但 Claude Code 自己把完整记录落在会话 home 里，而会话 home
// 是宿主机 bind mount，所以我们不用 hook、不用反代、不用进容器，读文件就行：
//
//	<会话home>/.claude/projects/<cwd 派生的目录>/<provider会话id>.jsonl
//
// 每条 assistant 记录带 `usage`（**是最终值**，不是流式事件里那个恒为 2 的
// output_tokens 占位）、`requestId`，以及关键的 `entrypoint`：
//
//	sdk-cli  —— claude -p，也就是我们自己发起的对话/起标题，已经记过账，跳过
//	cli      —— 用户在终端里手敲的 TUI，正是这里要补的
//
// 补记的行只记账不扣额度（见 store.UpsertTerminalUsage 的说明）。

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"agentbox/internal/agent"
	"agentbox/internal/config"
	"agentbox/internal/store"
)

const (
	// termScanInterval 是兜底全量扫描的周期。inotify 才是主力（见 termWatchLoop），
	// 这一趟管的是它漏掉或起不来的情况：watch 有内核上限、会话目录可能在两次对齐
	// 之间才出现、进程重启后也得先补一轮。
	termScanInterval = time.Minute
	// termWatchSettle 是收到写入后等多久再扫。CLI 一个回合里会连着写很多行，
	// 每行都扫一次纯属浪费；等它静一下再读，一次就把这段读完。
	termWatchSettle = 700 * time.Millisecond
	// termWatchRefresh 是重新对齐监听目录的周期：会话新建/删除、CLI 第一次在某个
	// cwd 下开工（新建 projects/<slug>/）都会改变该监听哪些目录。
	termWatchRefresh = 30 * time.Second
)

// termEntrypointCLI 是 Claude Code 给交互式 TUI 打的标记。`sdk-cli` 是 -p
// 无头模式（我们自己发起的那些），必须排除，否则对话消耗会被记两遍。
const termEntrypointCLI = "cli"

// termTranscriptRec 是 transcript 里我们关心的那几个字段。
type termTranscriptRec struct {
	Type       string `json:"type"`
	Entrypoint string `json:"entrypoint"`
	UUID       string `json:"uuid"`
	RequestID  string `json:"requestId"`
	Timestamp  string `json:"timestamp"`
	Message    struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// termTurn 是补记的单位：终端里的一次提问及其后续所有 API 调用，按模型分开。
// 与对话行的粒度对齐（一行 = 一个回合 × 一个模型），这样两种来源的行放在同一
// 张表里才可比。
type termTurn struct {
	reqID string // 本回合首个 requestId，去重键
	model string
	ts    time.Time
	ev    store.UsageEvent
}

// termScanner 扫描进度：路径 → 上次看到的大小与修改时间。文件没动过就整个跳过，
// 免得每趟把所有 transcript 重读一遍。只在内存里，重启后重扫一轮——重扫是安全
// 的（靠 req_id 去重），代价只是一次多余的读。
//
// 定时器和「打开使用记录页」两条路都会触发扫描，所以状态要上锁；锁同时保证同一
// 时刻只有一趟扫描在跑，两边撞上时后来的直接等前一趟的结果。
type Scanner struct {
	mu   sync.Mutex
	seen map[string]termFileState
}

type termFileState struct {
	size  int64
	mtime time.Time
}

// termUsageLoop 常驻补记终端消耗。用户不看页面时也得记——终端消耗要能在报表里
// 回溯，不能只在有人打开页面的那一刻才存在。
func (s *Service) termUsageLoop() {
	for s.ctx.Err() == nil {
		s.fullScan()
		// Coalesce refresh requests and cap full scans at one per five seconds.
		if !waitInterval(s.ctx, 5*time.Second) {
			return
		}
		timer := time.NewTimer(termScanInterval - 5*time.Second)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-s.request:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// scanTerminalUsage 遍历库里所有会话的 transcript，把终端消耗补进流水。
//
// 只走库里存在的会话：磁盘上会留下已删除会话的目录，那些消耗归不到人头上，
// 补了也只是往报表里塞孤儿行。
func (s *Service) scanTerminalUsage(sc *Scanner) {
	for _, sess := range s.store.All() {
		if s.workContext().Err() != nil {
			return
		}
		s.scanSessionUsage(sc, sess)
	}
}

// scanSessionUsage 补记一个会话的终端消耗。inotify 命中时只扫这一个会话，
// 不必为了一次写入把所有人的 transcript 都摸一遍。
func (s *Service) scanSessionUsage(sc *Scanner, sess store.Session) {
	adapter, err := agent.Lookup(sess.Agent)
	if err != nil || !adapter.Capabilities().TerminalUsage {
		return
	}
	dir := filepath.Join(s.homeDir(sess), ".claude", "projects")
	pattern := "*/*.jsonl"
	parser := parseTerminalReader
	if sess.Agent == config.AgentCodex {
		dir = filepath.Join(s.homeDir(sess), ".codex", "sessions")
		pattern = "*/*/*/rollout-*.jsonl"
		parser = parseCodexTerminal
	}
	root, err := s.openDataDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.scanErrors.Add(1)
		}
		return
	}
	defer root.Close()
	files, err := fs.Glob(root.FS(), pattern)
	if err != nil {
		s.scanErrors.Add(1)
	}
	if err != nil || len(files) == 0 {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for _, rel := range files {
		if s.ctx.Err() != nil {
			return
		}
		path := filepath.Join(dir, filepath.FromSlash(rel))
		f, err := root.OpenFile(rel)
		if err != nil {
			s.scanErrors.Add(1)
			continue
		}
		fi, err := f.Stat()
		if err != nil {
			s.scanErrors.Add(1)
			f.Close()
			continue
		}
		if st, ok := sc.seen[path]; ok && st.size == fi.Size() && st.mtime.Equal(fi.ModTime()) {
			f.Close()
			continue
		}
		turns, err := parser(f)
		f.Close()
		if err != nil {
			s.scanErrors.Add(1)
			log.Printf("terminal usage %s: %v", path, err)
			continue
		}
		// 先记下这次的状态：解析失败上面已经跳过，走到这里说明文件读完了，
		// 下一轮没改动就不用再读。
		sc.seen[path] = termFileState{size: fi.Size(), mtime: fi.ModTime()}
		if len(turns) == 0 {
			continue
		}
		evs := make([]store.UsageEvent, 0, len(turns))
		for _, t := range turns {
			ev := t.ev
			ev.TS = t.ts
			ev.User, ev.SessionID = sess.User, sess.ID
			ev.Agent, ev.AccountID = sess.Agent, sess.AccountID
			ev.Model, ev.ReqID = t.model, t.reqID
			if sess.Agent == config.AgentCodex {
				ev.ReqID = sess.ID + ":" + ev.ReqID
				ev.TurnID = sess.ID + ":" + ev.TurnID
			}
			ev.Kind = store.UsageKindTerminal
			// 终端行的价只能查表：transcript 给 token 不给美元。查不到就是 0，
			// 「未定价」如实显示，不假装算得出来。
			ev = s.Price(ev)
			evs = append(evs, ev)
		}
		if err := s.store.UpsertTerminalUsage(evs...); err != nil {
			s.scanErrors.Add(1)
			log.Printf("terminal usage %s: %v", path, err)
			// 落库失败就把状态撤回，下一轮重试；不然这个文件要等到下次被改
			// 动才会再看一眼，中间的消耗就永久漏了。
			delete(sc.seen, path)
		}
	}
}

// termWatchLoop 用 inotify 盯住各会话的 transcript 目录，CLI 一写完就补记，
// 把终端消耗出现在流水里的延迟从「最多一分钟」压到「不到一秒」。
//
// 会话 home 是宿主机 bind mount，容器里写进去的内容走的是同一个内核 VFS，
// 所以宿主机这边照样收得到事件（实测 CREATE/WRITE/REMOVE 都到）。
//
// 起不来就直接退出，不影响 termUsageLoop 那趟定时全量扫描——延迟退化回一分钟，
// 但一条账都不会少。
func (s *Service) termWatchLoop() {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("terminal usage: inotify 起不来，退回定时扫描：%v", err)
		return
	}
	defer w.Close()

	watched := map[string]string{} // 监听中的目录 -> 会话 id
	dirty := map[string]bool{}     // 待补记的会话
	s.syncTermWatches(w, watched)

	refresh := time.NewTicker(termWatchRefresh)
	defer refresh.Stop()
	// settle 非 nil 表示「这一波写入还在攒着」。只在攒的第一下开表、后续写入不再
	// 延后，否则一个持续写文件的长回合会把补记无限推迟。
	var settle <-chan time.Time

	for {
		select {
		case <-s.workContext().Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if !strings.HasSuffix(ev.Name, ".jsonl") {
				// projects/ 底下新建的 cwd 目录：把它也纳入监听，
				// 否则用户在新目录里第一次开工我们收不到事件。
				if ev.Has(fsnotify.Create) {
					s.syncTermWatches(w, watched)
				}
				continue
			}
			if id := watched[filepath.Dir(ev.Name)]; id != "" {
				dirty[id] = true
				if settle == nil {
					settle = time.After(termWatchSettle)
				}
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("terminal usage watch: %v", err)
		case <-settle:
			settle = nil
			for id := range dirty {
				delete(dirty, id)
				if sess, ok := s.store.Get(id); ok {
					s.scanSessionUsage(s.termScan, sess)
				}
			}
		case <-refresh.C:
			s.syncTermWatches(w, watched)
		}
	}
}

// syncTermWatches 让监听的目录集合与库里的支持终端计量的会话对齐：新会话加上、
// 删掉的会话摘掉。projects/ 本身也要监听——CLI 换个 cwd 就会在底下新建目录。
func (s *Service) syncTermWatches(w *fsnotify.Watcher, watched map[string]string) {
	want := map[string]string{}
	for _, sess := range s.store.All() {
		adapter, err := agent.Lookup(sess.Agent)
		if err != nil || !adapter.Capabilities().TerminalUsage {
			continue
		}
		root := filepath.Join(s.homeDir(sess), ".claude", "projects")
		depth := 1
		if sess.Agent == config.AgentCodex {
			root = filepath.Join(s.homeDir(sess), ".codex", "sessions")
			depth = 3
		}
		area, err := s.openDataDir(root)
		if err != nil {
			continue
		}
		// Walk through confined handles. Watch only the provider's expected
		// date/cwd depth; symlink directories are never traversed.
		_ = fs.WalkDir(area.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			level := 0
			if rel != "." {
				level = len(strings.Split(rel, "/"))
			}
			if level > depth {
				return fs.SkipDir
			}
			want[filepath.Join(root, filepath.FromSlash(rel))] = sess.ID
			return nil
		})
		area.Close()
	}
	for dir := range watched {
		if _, ok := want[dir]; !ok {
			_ = w.Remove(dir)
			delete(watched, dir)
		}
	}
	for dir, id := range want {
		if _, ok := watched[dir]; ok {
			continue
		}
		if err := w.Add(dir); err != nil {
			// watch 数有内核上限（fs.inotify.max_user_watches），加不上不是灾难：
			// 定时全量扫描仍然覆盖这个会话，只是延迟回到一分钟。
			log.Printf("terminal usage watch %s: %v", dir, err)
			continue
		}
		watched[dir] = id
	}
}

// parseTerminalTranscript 从一个 transcript 文件里读出终端回合。
//
// 按文件顺序流式聚合：遇到 user 记录就开一个新回合，其后的 assistant 记录按模型
// 累加进去。同一个 message.id 会在文件里出现多次，优先完整 stop_reason，
// 同等完整度取 output 较大者，避免累加或用晚到占位覆盖最终记录。
func parseTerminalReader(f io.Reader) ([]termTurn, error) {
	// 按 (回合, 模型) 归并，同时记住出场顺序——报表里回合按时间排，靠这个稳定。
	type key struct{ turn, model string }
	acc := map[key]*termTurn{}
	var order []key
	// 同一消息选一份最终用量，读完文件后再累加。
	type reqUsage struct {
		k       key
		ev      store.UsageEvent
		reqID   string
		ts      time.Time
		message claudeMessage
	}
	reqs := map[string]*reqUsage{}
	var reqOrder []string

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 32<<20) // transcript 单行可以很大（含附件）
	turnUUID := ""
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec termTranscriptRec
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Entrypoint != termEntrypointCLI {
			continue // -p 发起的那些已经记过账了
		}
		switch rec.Type {
		case "user":
			// 一次提问开一个回合。user 记录一定有 uuid，promptId 在老版本 CLI 上
			// 是 null，所以用 uuid 当回合键。
			if rec.UUID != "" {
				turnUUID = rec.UUID
			}
		case "assistant":
			u := rec.Message.Usage
			id := rec.Message.ID
			if id == "" {
				id = rec.RequestID
			} // legacy transcript compatibility
			if id == "" || strings.HasPrefix(rec.Message.Model, "<") {
				continue
			}
			if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 {
				continue
			}
			k := key{turn: turnUUID, model: rec.Message.Model}
			message := claudeMessage{ID: id, Model: rec.Message.Model, StopReason: rec.Message.StopReason, Usage: claudeTokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}}
			r, ok := reqs[id]
			if !ok {
				r = &reqUsage{k: k, reqID: rec.RequestID}
				if r.reqID == "" {
					r.reqID = id
				}
				reqs[id] = r
				reqOrder = append(reqOrder, id)
			}
			if ok && !preferClaudeMessage(message, r.message) {
				continue
			}
			r.message = message
			// Final snapshots win; a later placeholder cannot reduce usage.
			r.ev = store.UsageEvent{
				InputTokens: u.Input, OutputTokens: u.Output,
				CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite,
			}
			r.k = k
			// 时间相反，取最早那份：重复记录是同一次调用流式写了两遍，
			// 这次调用真正发生的时刻是第一条。
			if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil && r.ts.IsZero() {
				r.ts = t
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	for _, id := range reqOrder {
		r := reqs[id]
		t, ok := acc[r.k]
		if !ok {
			t = &termTurn{reqID: r.reqID, model: r.k.model, ts: r.ts}
			acc[r.k] = t
			order = append(order, r.k)
		}
		t.ev.InputTokens += r.ev.InputTokens
		t.ev.OutputTokens += r.ev.OutputTokens
		t.ev.CacheReadTokens += r.ev.CacheReadTokens
		t.ev.CacheWriteTokens += r.ev.CacheWriteTokens
		if t.ts.IsZero() || (!r.ts.IsZero() && r.ts.Before(t.ts)) {
			t.ts = r.ts // 回合时间取最早那次调用，和对话行的「回合开始」对齐
		}
	}

	// 回合 ID 用这一回合最早出现的 requestId：同一回合按模型拆出的多行共享它，
	// 与对话行「同回合共享 turn_id」的语义一致，报表里数回合才不会重复计数。
	out := make([]termTurn, 0, len(order))
	firstReq := map[string]string{}
	for _, k := range order {
		t := *acc[k]
		if _, ok := firstReq[k.turn]; !ok {
			firstReq[k.turn] = t.reqID
		}
		t.ev.TurnID = firstReq[k.turn]
		out = append(out, t)
	}
	return out, nil
}
