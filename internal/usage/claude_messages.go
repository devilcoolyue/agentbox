package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

type claudeTokens struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}
type claudeMessage struct {
	ID         string       `json:"id"`
	Model      string       `json:"model"`
	StopReason string       `json:"stop_reason,omitempty"`
	Usage      claudeTokens `json:"usage"`
	// TS is when the response first showed up (Unix ms): the stream's arrival
	// time or the transcript timestamp, whichever is earlier. Provider message
	// objects carry no such field, so only we set it; old rows lack it.
	TS   int64  `json:"ts,omitempty"`
	kind string // empty is the turn's own chat spend
}

// claudeTotal is one model's cumulative counters in result.modelUsage and the
// transcript's cost-state. They include resumed history, so only a difference
// within the same process is ever billed.
type claudeTotal struct {
	Input      int64 `json:"inputTokens"`
	Output     int64 `json:"outputTokens"`
	CacheRead  int64 `json:"cacheReadInputTokens"`
	CacheWrite int64 `json:"cacheCreationInputTokens"`
}

// Match cc-switch's selection rule: a completed snapshot wins, then the
// greatest output count. Trailing placeholders must never replace final usage.
func preferClaudeMessage(next, old claudeMessage) bool {
	if (next.StopReason != "") != (old.StopReason != "") {
		return next.StopReason != ""
	}
	return next.Usage.Output > old.Usage.Output
}

type claudeMeter struct {
	base        store.UsageEvent
	messages    map[string]claudeMessage
	streams     map[string]claudeMessage
	providers   map[string]string
	sessionID   string
	durationMS  int64
	home        string
	before      map[string]fs.FileInfo
	transcripts bool
	done        bool
	// suggest bills the prompt-suggestion request the CLI makes after result:
	// it reaches neither stdout nor the transcript's messages, only the final
	// cost-state, so it is the growth from atResult to that record.
	suggest  bool
	atResult map[string]claudeTotal
	final    map[string]claudeTotal
	// Requests the CLI makes without reporting them per message (/compact's
	// summary, automatic compaction, WebFetch's page digest) show up only in
	// its cumulative totals. addHidden bills result.modelUsage less what the
	// process restored at start (restored, the last cost-state before this
	// turn) less the messages this process was seen making (live). history
	// says the transcript predates the turn; finalStart and the restored
	// startTime match when the CLI really carried the totals over.
	live       map[string]bool
	mainSeen   bool
	history    bool
	restored   *costState
	finalStart int64
	compacted  bool
}

// costState is the cumulative record the CLI appends to the transcript as a
// process exits; a resumed process restores it, startTime included.
type costState struct {
	Type       string                 `json:"type"`
	StartTime  int64                  `json:"startTime"`
	ModelUsage map[string]claudeTotal `json:"modelUsage"`
}

func newClaudeMeter() *claudeMeter {
	return &claudeMeter{messages: map[string]claudeMessage{}, streams: map[string]claudeMessage{}, providers: map[string]string{}, live: map[string]bool{}}
}
func (c *claudeMeter) add(m claudeMessage) {
	u := m.Usage
	if m.ID == "" || m.Model == "" || strings.HasPrefix(m.Model, "<") || u.Input < 0 || u.Output < 0 || u.CacheRead < 0 || u.CacheWrite < 0 {
		return
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 {
		return
	}
	old, ok := c.messages[m.ID]
	if !ok {
		c.messages[m.ID] = m
		return
	}
	ts := earliest(old.TS, m.TS)
	if preferClaudeMessage(m, old) {
		old = m
	}
	old.TS = ts
	c.messages[m.ID] = old
}

func earliest(a, b int64) int64 {
	if a == 0 || b != 0 && b < a {
		return b
	}
	return a
}
func (c *claudeMeter) observe(base store.UsageEvent, line []byte) {
	if c.done {
		return
	}
	c.base = base
	var e struct {
		Type      string        `json:"type"`
		Subtype   string        `json:"subtype"`
		SessionID string        `json:"session_id"`
		Parent    string        `json:"parent_tool_use_id"`
		Message   claudeMessage `json:"message"`
		Event     struct {
			Type    string          `json:"type"`
			Message claudeMessage   `json:"message"`
			Usage   json.RawMessage `json:"usage"`
			Delta   struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		} `json:"event"`
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	if e.SessionID != "" && e.Parent == "" {
		c.sessionID = e.SessionID
	}
	now := time.Now().UnixMilli()
	switch e.Type {
	case "system":
		if e.Subtype == "compact_boundary" {
			c.compacted = true
		}
	case "assistant":
		e.Message.TS = now
		c.add(e.Message)
	case "result":
		var r claudeResult
		if json.Unmarshal(line, &r) == nil {
			c.durationMS = r.DurationMS
			c.atResult = map[string]claudeTotal{}
			for model, u := range r.ModelUsage {
				c.providers[model] = u.Provider
				c.atResult[model] = claudeTotal{u.Input, u.Output, u.CacheRead, u.CacheWrite}
			}
		}
		// modelUsage / total_cost_usd may include earlier resumed turns.
		// They are metadata only here, never a fallback billing source.
	case "stream_event":
		switch e.Event.Type {
		case "message_start":
			e.Event.Message.TS = now
			c.streams[e.Parent] = e.Event.Message
			c.live[e.Event.Message.ID] = true
			c.add(e.Event.Message)
		case "message_delta":
			m, ok := c.streams[e.Parent]
			if !ok {
				return
			}
			// Unmarshal onto the existing usage: omitted input/cache counters
			// stay intact while output is replaced, not summed across deltas.
			if len(e.Event.Usage) > 0 && json.Unmarshal(e.Event.Usage, &m.Usage) != nil {
				return
			}
			if e.Event.Delta.StopReason != "" {
				m.StopReason = e.Event.Delta.StopReason
			}
			c.streams[e.Parent] = m
			c.add(m)
		case "message_stop":
			delete(c.streams, e.Parent)
		}
	}
}

// NewChatTally pins pricing and transcript offsets BEFORE launching the CLI.
// Reading only new bytes excludes earlier turns (including pre-upgrade data)
// and avoids trusting timestamps or cumulative counters to identify requests.
func (s *Service) NewChatTally(sess store.Session) Tally {
	t := s.NewTally()
	if sess.Agent != config.AgentClaude {
		return t
	}
	c := newClaudeMeter()
	t.claude = c
	c.home = s.homeDir(sess)
	c.before = map[string]fs.FileInfo{}
	root, err := s.openDataDir(filepath.Join(c.home, ".claude", "projects"))
	if errors.Is(err, fs.ErrNotExist) {
		c.transcripts = true
		return t
	}
	if err != nil {
		log.Printf("usage transcript baseline %s: %v", sess.ID, err)
		return t
	}
	defer root.Close()
	for _, pattern := range []string{"*/*.jsonl", "*/*/subagents/*.jsonl"} {
		files, err := fs.Glob(root.FS(), pattern)
		if err != nil {
			return t
		}
		for _, path := range files {
			info, err := root.Lstat(path)
			if err != nil {
				return t
			}
			c.before[path] = info
		}
	}
	c.transcripts = true
	return t
}

var claudeSessionIDRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func (s *Service) completeClaudeMessages(c *claudeMeter) {
	if !c.transcripts || !claudeSessionIDRE.MatchString(c.sessionID) {
		return
	}
	root, err := s.openDataDir(filepath.Join(c.home, ".claude", "projects"))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("usage transcript %s: %v", c.base.SessionID, err)
		return
	}
	defer root.Close()
	for _, pattern := range []string{"*/" + c.sessionID + ".jsonl", "*/" + c.sessionID + "/subagents/*.jsonl"} {
		files, err := fs.Glob(root.FS(), pattern)
		if err != nil {
			log.Printf("usage transcript: %v", err)
			continue
		}
		for _, path := range files {
			f, err := root.OpenFile(path)
			if err != nil {
				log.Printf("usage transcript: %v", err)
				continue
			}
			child := strings.Contains(path, "/subagents/")
			if before := c.before[path]; !child {
				c.mainSeen = true
				if before != nil {
					c.history = true
					if c.restored, err = lastCostState(f, before.Size()); err != nil {
						log.Printf("usage transcript %s: %v", c.base.SessionID, err)
					}
				}
			}
			err = c.readTranscript(f, c.before[path], child)
			f.Close()
			if err != nil {
				log.Printf("usage transcript %s: %v", c.base.SessionID, err)
			}
		}
	}
}
func (c *claudeMeter) readTranscript(f *os.File, before fs.FileInfo, child bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var offset int64
	skipPartial := false
	if before != nil {
		if !os.SameFile(before, info) || info.Size() < before.Size() {
			return fmt.Errorf("transcript replaced or truncated")
		}
		offset = before.Size()
		if offset > 0 {
			var tail [1]byte
			if _, err = f.ReadAt(tail[:], offset-1); err != nil {
				return err
			}
			// Do not attribute a pre-existing unfinished record to this turn.
			skipPartial = tail[0] != '\n'
		}
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(io.LimitReader(f, info.Size()-offset))
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		if skipPartial {
			skipPartial = false
			continue
		}
		var rec struct {
			Type       string                 `json:"type"`
			Entrypoint string                 `json:"entrypoint"`
			Timestamp  string                 `json:"timestamp"`
			Message    claudeMessage          `json:"message"`
			StartTime  int64                  `json:"startTime"`
			ModelUsage map[string]claudeTotal `json:"modelUsage"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec.Type == "cost-state" && !child && rec.ModelUsage != nil {
			c.final = rec.ModelUsage // the last one is written as the CLI exits
			c.finalStart = rec.StartTime
			continue
		}
		if rec.Type != "assistant" {
			continue
		}
		if rec.Entrypoint != "sdk-cli" && !(child && rec.Entrypoint == "") {
			continue
		}
		rec.Message.TS = 0
		if ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
			rec.Message.TS = ts.UnixMilli()
		}
		if rec.Message.ID != "" {
			c.live[rec.Message.ID] = true
		}
		c.add(rec.Message)
	}
	return sc.Err()
}

// costStateScan bounds the backwards search: one cost-state closes every
// finished process, so the newest sits near the end unless a process died.
const costStateScan = 16 << 20

// lastCostState returns the newest cost-state record that ends before end,
// or nil when there is none within costStateScan bytes.
func lastCostState(f *os.File, end int64) (*costState, error) {
	const chunk = 256 << 10
	var carry []byte
	for pos := end; pos > 0 && end-pos < costStateScan; {
		n := min(int64(chunk), pos)
		pos -= n
		buf := make([]byte, n, n+int64(len(carry)))
		if _, err := f.ReadAt(buf, pos); err != nil {
			return nil, err
		}
		lines := bytes.Split(append(buf, carry...), []byte{'\n'})
		first := 0
		carry = nil
		if pos > 0 {
			// The first piece may continue in the bytes before it.
			carry, first = lines[0], 1
		}
		for i := len(lines) - 1; i >= first; i-- {
			if !bytes.Contains(lines[i], []byte(`"cost-state"`)) {
				continue
			}
			var rec costState
			if json.Unmarshal(lines[i], &rec) == nil && rec.Type == "cost-state" && rec.ModelUsage != nil {
				return &rec, nil
			}
		}
	}
	return nil, nil
}

// addHidden bills what the CLI's own totals show this turn spent beyond the
// messages it was seen making. Nothing is guessed: a resumed session needs
// both cost-state records to tell restored history from this turn, older
// CLIs that write none are skipped, and so is anything that does not add up.
func (c *claudeMeter) addHidden() {
	if c.atResult == nil || !c.mainSeen {
		return
	}
	base := map[string]claudeTotal{}
	if c.history {
		if c.restored == nil || c.final == nil {
			if c.compacted {
				log.Printf("usage: 会话 %s 压缩了上下文，但找不到前后的累计用量记录，压缩请求的消耗不会入账", c.base.SessionID)
			}
			return
		}
		for model, at := range c.atResult {
			end := c.final[model]
			if end.Input < at.Input || end.Output < at.Output || end.CacheRead < at.CacheRead || end.CacheWrite < at.CacheWrite {
				log.Printf("usage: 会话 %s 退出时的累计用量少于回合结束时（%s），不核对未逐条上报的请求", c.base.SessionID, model)
				return
			}
		}
		if c.finalStart == c.restored.StartTime {
			base = c.restored.ModelUsage
		}
	}
	counted := map[string]claudeTokens{}
	for id, m := range c.messages {
		if m.kind != "" || !c.live[id] {
			continue // replayed after compaction, or a later kind
		}
		model, ok := totalsModel(m.Model, c.atResult)
		if !ok {
			log.Printf("usage: 会话 %s 的消息模型 %s 对不上 CLI 的累计用量，不核对未逐条上报的请求", c.base.SessionID, m.Model)
			return
		}
		t := counted[model]
		t.Input += m.Usage.Input
		t.Output += m.Usage.Output
		t.CacheRead += m.Usage.CacheRead
		t.CacheWrite += m.Usage.CacheWrite
		counted[model] = t
	}
	for model, after := range c.atResult {
		before, seen := base[model], counted[model]
		u := claudeTokens{after.Input - before.Input - seen.Input, after.Output - before.Output - seen.Output,
			after.CacheRead - before.CacheRead - seen.CacheRead, after.CacheWrite - before.CacheWrite - seen.CacheWrite}
		if u.Input < 0 || u.Output < 0 || u.CacheRead < 0 || u.CacheWrite < 0 {
			log.Printf("usage: 会话 %s 的累计用量少于逐条记到的（%s），不核对未逐条上报的请求", c.base.SessionID, model)
			continue
		}
		id := "cli-hidden:" + c.base.TurnID + ":" + model
		c.live[id] = true
		c.add(claudeMessage{ID: id, Model: model, Usage: u, TS: time.Now().UnixMilli()})
	}
}

var modelSuffixRE = regexp.MustCompile(`(-\d{8}|\[\d+[mM]\])$`)

// totalsModel finds the modelUsage key a message's model is counted under:
// the same name, the name without a date or context suffix, or the only key.
func totalsModel(model string, totals map[string]claudeTotal) (string, bool) {
	if _, ok := totals[model]; ok {
		return model, true
	}
	bare := modelSuffixRE.ReplaceAllString(model, "")
	for key := range totals {
		if modelSuffixRE.ReplaceAllString(key, "") == bare {
			return key, true
		}
	}
	if len(totals) == 1 {
		for key := range totals {
			return key, true
		}
	}
	return "", false
}

// addSuggestion adds what the CLI spent after result, keyed by turn so a
// replay cannot charge it twice. Without both totals nothing is guessed.
func (c *claudeMeter) addSuggestion() {
	if !c.suggest || c.atResult == nil {
		return
	}
	if c.final == nil {
		log.Printf("usage: 会话 %s 请求了下一步提示，但没有找到累计用量记录，这次提示的消耗不会入账", c.base.SessionID)
		return
	}
	for model, after := range c.final {
		before := c.atResult[model]
		u := claudeTokens{after.Input - before.Input, after.Output - before.Output, after.CacheRead - before.CacheRead, after.CacheWrite - before.CacheWrite}
		if u.Input < 0 || u.Output < 0 || u.CacheRead < 0 || u.CacheWrite < 0 {
			log.Printf("usage: 会话 %s 的累计用量倒退（%s），下一步提示的消耗不入账", c.base.SessionID, model)
			continue
		}
		c.add(claudeMessage{ID: "prompt-suggestion:" + c.base.TurnID + ":" + model, Model: model, Usage: u, TS: time.Now().UnixMilli(), kind: store.UsageKindSuggestion})
	}
}

func (s *Service) flushClaudeMessages(t *Tally, wall time.Duration) int {
	c := t.claude
	if c.done {
		return 0
	}
	s.completeClaudeMessages(c)
	c.addHidden()
	c.addSuggestion()
	// One row per provider response, in the order the responses happened.
	// Rows from transcripts that predate per-message timestamps sort by ID.
	ids := make([]string, 0, len(c.messages))
	for id := range c.messages {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := c.messages[ids[i]], c.messages[ids[j]]
		if a.TS != b.TS {
			return a.TS != 0 && (b.TS == 0 || a.TS < b.TS)
		}
		return ids[i] < ids[j]
	})
	plan := t.pricing
	if plan == nil {
		p := s.cfg.PricingPlan()
		plan = &p
	}
	// Turn latency goes on the turn's first chat request this process made;
	// a message replayed after compaction is history, already billed.
	timed := ""
	for _, id := range ids {
		if c.messages[id].kind == "" && (timed == "" || c.live[id]) {
			timed = id
			if c.live[id] {
				break
			}
		}
	}
	now := time.Now()
	evs := make([]store.UsageEvent, 0, len(ids))
	for _, id := range ids {
		m := c.messages[id]
		e := c.base
		e.Model = m.Model
		e.ReqID = id
		e.InputTokens = m.Usage.Input
		e.OutputTokens = m.Usage.Output
		e.CacheReadTokens = m.Usage.CacheRead
		e.CacheWriteTokens = m.Usage.CacheWrite
		// Turn latency is measured once per turn; it sits on one chat request
		// and stays 0 on the rest, so MAX per turn still holds.
		if id == timed {
			e.DurationMS = c.durationMS
			e.WallMS = wall.Milliseconds()
		} else {
			e.TTFTMs, e.DurationMS, e.WallMS = 0, 0, 0
		}
		if m.kind != "" {
			e.Kind = m.kind
		}
		e.TS = now
		if m.TS != 0 {
			e.TS = time.UnixMilli(m.TS)
		}
		e.Provider = c.providers[m.Model]
		raw, _ := json.Marshal(m)
		e.Raw = string(raw)
		evs = append(evs, priceWith(*plan, e))
	}
	n, err := s.store.InsertUsageMessages(evs...)
	if err != nil {
		log.Printf("record Claude messages %s: %v", c.base.SessionID, err)
		t.settlementErr = err
		return 0
	}
	c.done = true
	t.settlementErr = nil
	return n
}
