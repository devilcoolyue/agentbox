package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"
	"unicode"
)

// threadIndex is what the thread list, history pages and title checks need
// from one thread file, kept so a request no longer reads and decodes the
// whole JSONL. Long threads are tens of megabytes of tool output: in 2026-10 a
// workspace's thread list decoded 52 files (104 MB) on every open, about 2.7 s,
// and every history page decoded its whole thread again.
//
// Thread files are only appended to, whole lines at a time under the room's
// fileMu (history cursors rely on the same property), so an index extends by
// decoding only the bytes after the last newline it consumed. A file that
// shrank, was replaced, or no longer has a newline where the index stopped is
// decoded again from the start. Line numbers match parseEntries: blank and
// invalid lines are not counted. Callers hold the session room's fileMu.
type threadIndex struct {
	file     os.FileInfo
	consumed int64 // bytes decoded, always up to and including a newline
	lines    []threadLine
	// mark is the start of the last indexed line (its timestamp makes it
	// unique), checked so a file recreated on a reused inode is not mistaken
	// for an append.
	mark []byte

	// threadMeta inputs, as scanThread derived them.
	ts, updated time.Time
	turns       int
	sent        bool
	resume      string
	first       string // first user message, already truncated for the fallback title
	title       string // newest non-empty title entry

	// threadTitleSource and outline inputs: user lines still counted as
	// messages (a reply the CLI produced locally removes the message before it).
	messages []threadMessage
	used     time.Time
}

type threadLine struct {
	off  int64 // offset of the trimmed line
	size int32 // trimmed length
	sent bool  // historySent
	user bool
	// slim is the copy a page sends for an unsent line (a Claude tool result),
	// kept once made: making it decodes the whole result, often a file's
	// contents, while the copy is small.
	slim     json.RawMessage
	slimDone bool
}

// decodeEntry is one parseEntries line. json.Unmarshal checks the whole input
// before decoding and reports a syntax error exactly when json.Valid is false,
// so the separate validity pass is not needed.
func decodeEntry(l []byte) (histEntry, bool) {
	e := histEntry{raw: json.RawMessage(l)}
	var syntax *json.SyntaxError
	if err := json.Unmarshal(l, &e.meta); errors.As(err, &syntax) {
		return histEntry{}, false
	}
	return e, true
}

// scan decodes the lines of data, which starts at file offset base.
func (x *threadIndex) scan(base int64, data []byte) {
	for pos := 0; pos < len(data); {
		end := bytes.IndexByte(data[pos:], '\n')
		if end < 0 {
			end = len(data)
		} else {
			end += pos
		}
		line := data[pos:end]
		lead := len(line) - len(bytes.TrimLeftFunc(line, unicode.IsSpace))
		trimmed := bytes.TrimRightFunc(line[lead:], unicode.IsSpace)
		if len(trimmed) > 0 {
			x.add(base+int64(pos+lead), trimmed)
		}
		pos = end + 1
	}
}

func (x *threadIndex) add(off int64, l []byte) {
	e, ok := decodeEntry(l)
	if !ok {
		return
	}
	sent, answer := historyEvent(&e)
	x.lines = append(x.lines, threadLine{off: off, size: int32(len(l)), sent: sent, user: e.meta.Kind == "user"})
	if x.ts.IsZero() && !e.meta.TS.IsZero() {
		x.ts = e.meta.TS
	}
	if !e.meta.TS.IsZero() {
		x.updated = e.meta.TS
	}
	switch e.meta.Kind {
	case "user":
		x.turns++
		x.sent = true
		if x.turns == 1 {
			x.first = truncRunes(e.meta.Text, 120)
		}
		x.messages = append(x.messages, threadMessage{line: len(x.lines) - 1, reply: -1})
	case "event":
		x.sent = true
		if n := len(x.messages); n > 0 && answer != noAnswer {
			x.messages[n-1].observe(len(x.lines)-1, answer)
		}
	case "status":
		if e.meta.Local && len(x.messages) > 0 {
			x.messages = x.messages[:len(x.messages)-1]
		}
	case "title":
		if e.meta.Text != "" {
			x.title = e.meta.Text
		}
	case "chat_session":
		x.resume = e.meta.Text
	}
}

// unchanged reports whether the file still holds what the index decoded: a
// newline where it stopped and the same start of its last line.
func (x *threadIndex) unchanged(f *os.File) bool {
	if x.consumed == 0 {
		return true
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], x.consumed-1); err != nil || b[0] != '\n' {
		return false
	}
	if len(x.lines) == 0 {
		return true
	}
	got := make([]byte, len(x.mark))
	_, err := f.ReadAt(got, x.lines[len(x.lines)-1].off)
	return err == nil && bytes.Equal(got, x.mark)
}

func (x *threadIndex) clone() *threadIndex {
	y := *x
	y.lines = append([]threadLine(nil), x.lines...)
	y.messages = append([]threadMessage(nil), x.messages...)
	return &y
}

func (x *threadIndex) meta(tid string) threadMeta {
	m := threadMeta{ID: tid, TS: x.ts, Updated: x.updated, Turns: x.turns, Resumable: x.resume != "", sent: x.sent, resume: x.resume}
	if x.title != "" {
		m.Title = truncRunes(x.title, 120)
	} else {
		m.Title = x.first
	}
	return m
}

// read returns the bytes of lines[from:to] and the file offset they start at.
func (x *threadIndex) read(f *os.File, from, to int) ([]byte, int64, error) {
	if from >= to {
		return nil, 0, nil
	}
	base := x.lines[from].off
	last := x.lines[to-1]
	buf := make([]byte, last.off+int64(last.size)-base)
	if _, err := f.ReadAt(buf, base); err != nil {
		return nil, 0, err
	}
	return buf, base, nil
}

func (x *threadIndex) entry(f *os.File, i int) (histEntry, error) {
	raw, _, err := x.read(f, i, i+1)
	if err != nil {
		return histEntry{}, err
	}
	e, _ := decodeEntry(raw)
	return e, nil
}

// slimCopy is what a page sends for unsent line i (nil sends nothing); raw is
// the line when the caller already read it.
func (x *threadIndex) slimCopy(f *os.File, i int, raw []byte) json.RawMessage {
	l := &x.lines[i]
	if !l.slimDone {
		var e histEntry
		if raw != nil {
			e, _ = decodeEntry(raw)
		} else if read, err := x.entry(f, i); err == nil {
			e = read
		} else {
			return nil
		}
		l.slim, l.slimDone = historyTool(&e), true
	}
	return l.slim
}

// pageSize is what line i adds to a page's byte budget: the line as written,
// or for a Claude tool result the slim copy sent in its place.
func (x *threadIndex) pageSize(f *os.File, i int) int {
	if l := &x.lines[i]; l.sent {
		return int(l.size)
	}
	return len(x.slimCopy(f, i, nil))
}

// pageStart picks the first line of the page ending before end; pages stop
// at a user message once the soft limits are reached (see historyLimits).
func (x *threadIndex) pageStart(f *os.File, end int, limits historyLimits) int {
	count, size, start := 0, 0, end
	for start > 0 {
		l := &x.lines[start-1]
		if l.sent {
			count++
		}
		size += x.pageSize(f, start-1)
		start--
		if count >= limits.MaxEntries || size >= limits.MaxBytes {
			break
		}
		if l.user && (count >= limits.Entries || size >= limits.Bytes) {
			break
		}
	}
	return start
}

// within reports whether lines[from:] fit count rendered entries and size bytes.
func (x *threadIndex) within(f *os.File, from, count, size int) bool {
	n, total := 0, 0
	for i := from; i < len(x.lines); i++ {
		if x.lines[i].sent {
			n++
		}
		total += x.pageSize(f, i)
		if n > count || total > size {
			return false
		}
	}
	return true
}

// page returns what a history page sends for lines[start:end]. Only the sent
// lines are read; tool results not yet slimmed are read one by one.
func (x *threadIndex) page(f *os.File, start, end int) ([]json.RawMessage, error) {
	lines := make([]json.RawMessage, 0, end-start+1)
	for i := start; i < end; {
		if !x.lines[i].sent {
			if tool := x.slimCopy(f, i, nil); tool != nil {
				lines = append(lines, tool)
			}
			i++
			continue
		}
		run := i
		for run < end && x.lines[run].sent {
			run++
		}
		buf, base, err := x.read(f, i, run)
		if err != nil {
			return nil, err
		}
		for ; i < run; i++ {
			l := &x.lines[i]
			lines = append(lines, buf[l.off-base:l.off-base+int64(l.size)])
		}
	}
	return lines, nil
}

// maxThreadIndexes bounds the cache; an index is a few dozen bytes per line
// plus the slim copies of the tool results pages have shown.
const maxThreadIndexes = 2048

type threadIndexCache struct {
	mu sync.Mutex
	m  map[string]*threadIndex
}

func (c *threadIndexCache) get(path string) *threadIndex {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[path]
}

func (c *threadIndexCache) put(path string, x *threadIndex) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*threadIndex{}
	}
	if _, ok := c.m[path]; !ok && len(c.m) >= maxThreadIndexes {
		oldest := ""
		for p, y := range c.m {
			if oldest == "" || y.used.Before(c.m[oldest].used) {
				oldest = p
			}
		}
		delete(c.m, oldest)
	}
	c.m[path] = x
}

func (c *threadIndexCache) drop(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, path)
}

// threadIndex returns the up-to-date index of a thread file and the file,
// open for reading lines; the caller closes it. Callers hold fileMu.
func (s *Server) threadIndex(path string) (*threadIndex, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		s.threadIndexes.drop(path)
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	x := s.threadIndexes.get(path)
	if x != nil && (!os.SameFile(x.file, fi) || fi.Size() < x.consumed) {
		x = nil
	}
	if x != nil && !x.unchanged(f) {
		x = nil
	}
	if x == nil {
		x = &threadIndex{}
	}
	x.file = fi
	x.used = time.Now()
	var tail []byte
	tailAt := x.consumed
	if fi.Size() > x.consumed {
		buf := make([]byte, fi.Size()-x.consumed)
		n, err := f.ReadAt(buf, x.consumed)
		if err != nil && n < len(buf) && !errors.Is(err, io.EOF) {
			f.Close()
			return nil, nil, err
		}
		buf = buf[:n]
		done := bytes.LastIndexByte(buf, '\n') + 1
		x.scan(x.consumed, buf[:done])
		x.consumed += int64(done)
		tailAt, tail = x.consumed, buf[done:]
		if n := len(x.lines); n > 0 {
			l := x.lines[n-1]
			x.mark = make([]byte, min(int(l.size), 64))
			if _, err := f.ReadAt(x.mark, l.off); err != nil {
				x.mark = nil
			}
		}
	}
	s.threadIndexes.put(path, x)
	// A last line without its newline still counts, as in parseEntries, but
	// only for this read: the index resumes from the newline once it arrives.
	if len(bytes.TrimSpace(tail)) > 0 {
		x = x.clone()
		x.scan(tailAt, tail)
	}
	return x, f, nil
}
