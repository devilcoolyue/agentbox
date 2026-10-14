package usage

import (
	"bufio"
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
}

func newClaudeMeter() *claudeMeter {
	return &claudeMeter{messages: map[string]claudeMessage{}, streams: map[string]claudeMessage{}, providers: map[string]string{}}
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
			err = c.readTranscript(f, c.before[path], strings.Contains(path, "/subagents/"))
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
			ModelUsage map[string]claudeTotal `json:"modelUsage"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec.Type == "cost-state" && !child && rec.ModelUsage != nil {
			c.final = rec.ModelUsage // the last one is written as the CLI exits
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
		c.add(rec.Message)
	}
	return sc.Err()
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
	now := time.Now()
	timed := false
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
		// Turn latency is measured once per turn; it sits on the turn's first
		// chat request and stays 0 on the rest, so MAX per turn still holds.
		if m.kind == "" && !timed {
			timed = true
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
