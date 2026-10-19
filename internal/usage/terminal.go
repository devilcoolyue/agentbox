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
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
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
	// termWatchSettle 是收到写入后等多久再扫。CLI 一次回复会连着写好几行（每个
	// 内容块一行），等它静一下再读，一次把这段读完。扫描只读新增的行（见
	// termFileState），代价很小，所以这里不必等太久。
	termWatchSettle = 300 * time.Millisecond
	// termParseIdle：文件这么久没动过，就丢掉它的续读状态（解析器与已写入行的
	// 指纹），只留大小与修改时间。之后再有写入就从头读一遍，与重启后一样。
	// 不丢的话，服务端会给每一份历史 transcript 都常驻一份解析状态。
	termParseIdle = 30 * time.Minute
	// termTailBytes 是续读点之前留作指纹的字节数，用来认出文件被改写过。
	termTailBytes = 64
	// termMaxLine 是单行上限；transcript 单行可以很大（含附件）。
	termMaxLine = 32 << 20
	// termWatchRefresh 是重新对齐监听目录的周期：会话新建/删除、CLI 第一次在某个
	// cwd 下开工（新建 projects/<slug>/）都会改变该监听哪些目录。
	termWatchRefresh = 30 * time.Second
)

// termEntrypointCLI 是 Claude Code 给交互式 TUI 打的标记。`sdk-cli` 是 -p
// 无头模式（我们自己发起的那些），必须排除，否则对话消耗会被记两遍。
const termEntrypointCLI = "cli"

// termTranscriptRec 是 transcript 里我们关心的那几个字段。
type termTranscriptRec struct {
	Type       string          `json:"type"`
	Entrypoint string          `json:"entrypoint"`
	UUID       string          `json:"uuid"`
	RequestID  string          `json:"requestId"`
	Timestamp  string          `json:"timestamp"`
	Effort     json.RawMessage `json:"effort"`
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
// termTurn 是终端补记的一行：一次 provider 请求。名字沿用按回合聚合的年代，
// reqID 是这一行的去重键，ev.TurnID 是同回合各行共享的回合键。
type termTurn struct {
	reqID string
	model string
	ts    time.Time
	ev    store.UsageEvent
}

// transcriptEffort accepts only recorded CLI effort levels. Unknown shapes
// must not discard the usage record or be replaced with today's CLI settings.
func transcriptEffort(raw json.RawMessage) string {
	var effort string
	if json.Unmarshal(raw, &effort) != nil {
		return ""
	}
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return effort
	default:
		return ""
	}
}

// Scanner 记着每份 transcript 读到了哪里：路径 → 续读状态。文件没动过就整个跳过；
// 动过就只读续读点之后新写完的行，再只把内容变了的行写库。状态只在内存里，重启
// 后从头重扫一轮——重扫是安全的（靠 req_id 去重），代价只是一次多余的读。
//
// 定时器、inotify 和「打开使用记录页」几条路都会触发扫描，所以状态要上锁。
type Scanner struct {
	mu   sync.Mutex
	seen map[string]*termFileState
	// gen 每趟全量扫描加一；扫到的文件记下当时的 gen，全量扫完后没被扫到的
	// （文件或会话已删除）从 seen 里清掉。
	gen uint64
}

func newScanner() *Scanner { return &Scanner{seen: map[string]*termFileState{}} }

type termFileState struct {
	size    int64
	mtime   time.Time
	visited uint64
	active  time.Time // 上次读到新内容的时刻；闲置久了丢掉下面的续读状态

	// 续读状态；parser 为 nil 时下次从头读。
	info    fs.FileInfo // 认出文件被换成了另一个（inode 变了）
	offset  int64       // 已交给 parser 的字节数，总在一行的末尾
	tail    []byte      // offset 之前最后 termTailBytes 个字节
	parser  termParser
	written map[string]termRowKey // 已落库的行：req_id → 写入时的内容
}

// termParser 逐行吃 transcript，rows 返回「把迄今吃下的内容整份解析一遍」会得到
// 的全部行。续读与整份重读因此结果相同。
type termParser interface {
	line([]byte)
	rows() []termTurn
}

// termRowKey 是一行落库内容的指纹：解析结果没变的行不必再写一遍。
type termRowKey struct {
	turnID, model, effort, effortControl string
	ts                                   int64
	input, output, cacheRead, cacheWrite int64
}

func (t termTurn) key() termRowKey {
	return termRowKey{
		turnID: t.ev.TurnID, model: t.model, effort: t.ev.Effort, effortControl: t.ev.EffortControl,
		ts: t.ts.UnixNano(), input: t.ev.InputTokens, output: t.ev.OutputTokens,
		cacheRead: t.ev.CacheReadTokens, cacheWrite: t.ev.CacheWriteTokens,
	}
}

// advance 把文件里还没读过的完整行交给 parser，返回到目前为止的全部行。续读的
// 前提是同一个文件、只追加过：换了 inode、比续读点还短、或者续读点之前的尾巴
// 对不上（被整份重写过），都从头再读，已写入行的记录也一并作废、全部重写一遍。
// 认不出的只有「原地、同长度、只改续读点之前较远处」这种改法——CLI 的 transcript
// 只追加，不会这样写。
func (st *termFileState) advance(f *os.File, fi fs.FileInfo, fresh func() termParser) ([]termTurn, error) {
	if st.parser == nil || !st.continues(f, fi) {
		st.parser, st.offset, st.tail, st.written = fresh(), 0, nil, map[string]termRowKey{}
	}
	st.info = fi
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return nil, err
	}
	n, err := feedLines(f, st.parser)
	if err != nil {
		return nil, err
	}
	st.offset += n
	st.tail = nil
	if keep := min(st.offset, termTailBytes); keep > 0 {
		st.tail = make([]byte, keep)
		if _, err := f.ReadAt(st.tail, st.offset-keep); err != nil {
			return nil, err
		}
	}
	return st.parser.rows(), nil
}

func (st *termFileState) continues(f *os.File, fi fs.FileInfo) bool {
	if st.info == nil || !os.SameFile(st.info, fi) || fi.Size() < st.offset {
		return false
	}
	if len(st.tail) == 0 {
		return st.offset == 0
	}
	buf := make([]byte, len(st.tail))
	if _, err := f.ReadAt(buf, st.offset-int64(len(buf))); err != nil {
		return false
	}
	return bytes.Equal(buf, st.tail)
}

// pending 挑出与上次写入时不同（或还没写过）的行。
func (st *termFileState) pending(rows []termTurn) []termTurn {
	var changed []termTurn
	for _, t := range rows {
		if old, ok := st.written[t.reqID]; !ok || old != t.key() {
			changed = append(changed, t)
		}
	}
	return changed
}

func (st *termFileState) drop() {
	st.info, st.offset, st.tail, st.parser, st.written = nil, 0, nil, nil, nil
}

// sweep 在一趟完整的全量扫描之后清理：这趟没扫到的文件忘掉，闲置太久的文件
// 丢掉续读状态。
func (sc *Scanner) sweep(gen uint64, now time.Time) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for path, st := range sc.seen {
		if st.visited != gen {
			delete(sc.seen, path)
		} else if st.parser != nil && now.Sub(st.active) > termParseIdle {
			st.drop()
		}
	}
}

// feedLines 把 r 里完整的行逐行交给 p，返回吃掉的字节数。没有换行结尾的最后一段
// 只在它本身已是一条完整 JSON 时才吃：CLI 写一条记录是一次追加「JSON + 换行」，
// 而 JSON 对象的真前缀不可能是合法 JSON，所以写到一半的行不会被当成整行；没吃的
// 留到下一轮从这行开头再读。
func feedLines(r io.Reader, p termParser) (int64, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var n int64
	var long []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			if len(long)+len(chunk) > termMaxLine {
				return n, bufio.ErrTooLong
			}
			long = append(long, chunk...)
			continue
		}
		line := chunk
		if long != nil {
			line = append(long, chunk...)
			long = nil
		}
		if err == io.EOF {
			if len(line) > 0 && json.Valid(line) {
				p.line(line)
				n += int64(len(line))
			}
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if len(line) > termMaxLine {
			return n, bufio.ErrTooLong
		}
		n += int64(len(line))
		p.line(bytes.TrimSuffix(line[:len(line)-1], []byte{'\r'}))
	}
}

// feedTranscript 把整份 transcript 交给 p。
func feedTranscript(r io.Reader, p termParser) error {
	_, err := feedLines(r, p)
	return err
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
	sc.mu.Lock()
	sc.gen++
	gen := sc.gen
	sc.mu.Unlock()
	for _, sess := range s.store.All() {
		if s.workContext().Err() != nil {
			return
		}
		s.scanSessionUsage(sc, sess)
	}
	sc.sweep(gen, time.Now())
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
	fresh := func() termParser { return newClaudeTermParser() }
	if sess.Agent == config.AgentCodex {
		dir = filepath.Join(s.homeDir(sess), ".codex", "sessions")
		pattern = "*/*/*/rollout-*.jsonl"
		fresh = func() termParser { return newCodexTermParser() }
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
		st := sc.seen[path]
		if st == nil {
			st = &termFileState{}
			sc.seen[path] = st
		}
		st.visited = sc.gen
		if st.size == fi.Size() && st.mtime.Equal(fi.ModTime()) {
			f.Close()
			continue
		}
		turns, err := st.advance(f, fi, fresh)
		f.Close()
		if err != nil {
			s.scanErrors.Add(1)
			log.Printf("terminal usage %s: %v", path, err)
			// 下一轮从头再读，不在读坏的地方接着读。
			st.drop()
			st.size, st.mtime = 0, time.Time{}
			continue
		}
		st.size, st.mtime, st.active = fi.Size(), fi.ModTime(), time.Now()
		changed := st.pending(turns)
		if len(changed) == 0 {
			continue
		}
		evs := make([]store.UsageEvent, 0, len(changed))
		for _, t := range changed {
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
			// 落库失败：读取进度留着，这几行不记成已写入，并让下一轮不跳过这个
			// 文件——不然要等它下次被改动才重试，中间的消耗就漏了。
			st.size = -1
			continue
		}
		for _, t := range changed {
			st.written[t.reqID] = t.key()
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

// parseTerminalReader 从一个 transcript 里读出终端补记的行，一次请求一行。
func parseTerminalReader(f io.Reader) ([]termTurn, error) {
	p := newClaudeTermParser()
	if err := feedTranscript(f, p); err != nil {
		return nil, err
	}
	return p.rows(), nil
}

// claudeTermParser 逐行解析 Claude Code 的 transcript。
//
// 遇到 user 记录就开一个新回合，其后的 assistant 记录各是一次请求。同一个
// message.id 会在文件里出现多次，优先完整 stop_reason，同等完整度取 output
// 较大者，避免累加或用晚到占位覆盖最终记录。
//
// 去重键是这次请求的 requestId。旧版按 (回合, 模型) 合并成一行、键是该组第一个
// requestId——正好等于新版该组第一次请求的键，所以重扫时第一次请求原地覆盖旧的
// 合并行，其余请求新增成行，不会重复记账。
type claudeTermParser struct {
	reqs     map[string]*claudeTermRequest // message.id → 这次请求选定的那份用量
	order    []string                      // message.id 首次出现的顺序
	turnUUID string
}

type claudeTermRequest struct {
	turn, model string
	reqID       string
	ts          time.Time
	effort      string
	message     claudeMessage
}

func newClaudeTermParser() *claudeTermParser {
	return &claudeTermParser{reqs: map[string]*claudeTermRequest{}}
}

func (p *claudeTermParser) line(line []byte) {
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var rec termTranscriptRec
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	if rec.Entrypoint != termEntrypointCLI {
		return // -p 发起的那些已经记过账了
	}
	switch rec.Type {
	case "user":
		// 一次提问开一个回合。user 记录一定有 uuid，promptId 在老版本 CLI 上
		// 是 null，所以用 uuid 当回合键。
		if rec.UUID != "" {
			p.turnUUID = rec.UUID
		}
	case "assistant":
		u := rec.Message.Usage
		id := rec.Message.ID
		if id == "" {
			id = rec.RequestID
		} // legacy transcript compatibility
		if id == "" || strings.HasPrefix(rec.Message.Model, "<") {
			return
		}
		if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 {
			return
		}
		message := claudeMessage{ID: id, Model: rec.Message.Model, StopReason: rec.Message.StopReason, Usage: claudeTokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}}
		r, ok := p.reqs[id]
		if !ok {
			r = &claudeTermRequest{reqID: rec.RequestID}
			if r.reqID == "" {
				r.reqID = id
			}
			p.reqs[id] = r
			p.order = append(p.order, id)
		}
		if ok && !preferClaudeMessage(message, r.message) {
			return
		}
		// Final snapshots win; a later placeholder cannot reduce usage.
		r.message = message
		r.effort = transcriptEffort(rec.Effort)
		r.turn, r.model = p.turnUUID, rec.Message.Model
		// 时间相反，取最早那份：重复记录是同一次调用流式写了两遍，
		// 这次调用真正发生的时刻是第一条。
		if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil && r.ts.IsZero() {
			r.ts = t
		}
	}
}

// rows 按请求首次出现的顺序输出。回合 ID 用这一回合最早出现的 requestId：同一
// 回合的各行共享它，与对话行「同回合共享 turn_id」的语义一致，报表里数回合才
// 不会重复计数。
func (p *claudeTermParser) rows() []termTurn {
	out := make([]termTurn, 0, len(p.order))
	firstReq := map[string]string{}
	for _, id := range p.order {
		r := p.reqs[id]
		if _, ok := firstReq[r.turn]; !ok {
			firstReq[r.turn] = r.reqID
		}
		u := r.message.Usage
		t := termTurn{reqID: r.reqID, model: r.model, ts: r.ts, ev: store.UsageEvent{
			TurnID:      firstReq[r.turn],
			InputTokens: u.Input, OutputTokens: u.Output,
			CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite,
			Effort: r.effort,
		}}
		if t.ev.Effort != "" {
			t.ev.EffortControl = "effort"
		}
		out = append(out, t)
	}
	return out
}
