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
	if !ok || preferClaudeMessage(m, old) {
		c.messages[m.ID] = m
	}
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
	switch e.Type {
	case "assistant":
		c.add(e.Message)
	case "result":
		var r claudeResult
		if json.Unmarshal(line, &r) == nil {
			c.durationMS = r.DurationMS
			for model, u := range r.ModelUsage {
				c.providers[model] = u.Provider
			}
		}
		// modelUsage / total_cost_usd may include earlier resumed turns.
		// They are metadata only here, never a fallback billing source.
	case "stream_event":
		switch e.Event.Type {
		case "message_start":
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
			Type       string        `json:"type"`
			Entrypoint string        `json:"entrypoint"`
			Message    claudeMessage `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "assistant" {
			continue
		}
		if rec.Entrypoint != "sdk-cli" && !(child && rec.Entrypoint == "") {
			continue
		}
		c.add(rec.Message)
	}
	return sc.Err()
}
func (s *Service) flushClaudeMessages(t *Tally, wall time.Duration) int {
	c := t.claude
	if c.done {
		return 0
	}
	s.completeClaudeMessages(c)
	ids := make([]string, 0, len(c.messages))
	for id := range c.messages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	plan := t.pricing
	if plan == nil {
		p := s.cfg.PricingPlan()
		plan = &p
	}
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
		e.DurationMS = c.durationMS
		e.WallMS = wall.Milliseconds()
		e.TS = time.Now()
		e.Provider = c.providers[m.Model]
		raw, _ := json.Marshal(m)
		e.Raw = string(raw)
		e = priceWith(*plan, e)
		e.Price.PerRequest = true
		evs = append(evs, e)
	}
	n, err := s.store.InsertUsageMessages(evs...)
	if err != nil {
		log.Printf("record Claude messages %s: %v", c.base.SessionID, err)
		return 0
	}
	c.done = true
	return n
}
