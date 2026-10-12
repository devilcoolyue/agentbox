package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"agentbox/internal/store"
)

// Extract only complete visible messages, never tools or reasoning blocks.
func conversationText(e histEntry) string {
	if e.meta.Kind == "user" {
		return "User: " + e.meta.Text
	}
	if e.meta.Kind != "event" {
		return ""
	}
	var v struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if json.Unmarshal(e.meta.Event, &v) != nil {
		return ""
	}
	if v.Type == "assistant" {
		var b []string
		for _, c := range v.Message.Content {
			if c.Type == "text" {
				b = append(b, c.Text)
			}
		}
		if len(b) > 0 {
			return "Assistant: " + strings.Join(b, "\n")
		}
	}
	if v.Type == "item.completed" && v.Item.Type == "agent_message" {
		return "Assistant: " + v.Item.Text
	}
	return ""
}

func threadContext(entries []histEntry, tid string, end int, include bool) store.CollaborationThreadContext {
	c := store.CollaborationThreadContext{ThreadID: tid, Anchor: end, Include: include}
	parts := []string{}
	size := 0
	if include {
		for i := end - 1; i >= 0; i-- {
			text := conversationText(entries[i])
			if text == "" {
				continue
			}
			if size+len(text)+2 > 24000 {
				c.Truncated = true
				break
			}
			parts = append(parts, text)
			size += len(text) + 2
		}
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	c.Text = strings.Join(parts, "\n\n")
	// Hash the exact file prefix as well as the selection, including metadata.
	h := sha256.New()
	for _, e := range entries[:end] {
		h.Write(e.raw)
		h.Write([]byte{'\n'})
	}
	if include {
		h.Write([]byte("include"))
	}
	c.SHA256 = hex.EncodeToString(h.Sum(nil))
	return c
}

func (s *Server) handleCollaborationContext(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	room := s.chat.room(sess.ID)
	room.fileMu.Lock()
	defer room.fileMu.Unlock()
	tid := r.URL.Query().Get("thread")
	if !threadIDRe.MatchString(tid) || tid != s.activeThread(sess) {
		writeCollaborationError(w, r, store.ErrChatConflict)
		return
	}
	raw, err := os.ReadFile(s.threadPath(sess, tid))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	entries := parseEntries(raw)
	writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(reqUser(r)), "context": threadContext(entries, tid, len(entries), r.URL.Query().Get("include") != "0")})
}

// Called while both room admission and fileMu are held through acceptance.
func (s *Server) validateCollaborationContext(sess store.Session, c *store.CollaborationThreadContext) error {
	if c == nil {
		return nil
	}
	if !threadIDRe.MatchString(c.ThreadID) || c.ThreadID != s.activeThread(sess) || !utf8.ValidString(c.Text) {
		return store.ErrChatInvalid
	}
	raw, err := os.ReadFile(s.threadPath(sess, c.ThreadID))
	if err != nil {
		return err
	}
	entries := parseEntries(raw)
	// A new message since preview means the user must preview the updated context.
	if c.Anchor != len(entries) {
		return store.ErrChatConflict
	}
	expected := threadContext(entries, c.ThreadID, c.Anchor, c.Include)
	if expected != *c {
		return store.ErrChatConflict
	}
	return nil
}

func (s *Server) collaborationHistory(sess store.Session, tid string, entries []histEntry, start, end int, latest bool) ([]json.RawMessage, error) {
	u, ok := s.store.GetUser(sess.User)
	if !ok {
		out := []json.RawMessage{}
		for _, e := range entries[start:end] {
			if historySent(e) {
				out = append(out, e.raw)
			}
		}
		return out, nil
	}
	runs, err := s.store.CollaborationThreadRuns(u, sess.ID, tid)
	if err != nil {
		return nil, err
	}
	at := map[int][]json.RawMessage{}
	for _, run := range runs {
		if run.Request == nil || run.Request.Context == nil {
			continue
		}
		anchor := run.Request.Context.Anchor
		if anchor < start || anchor > end || anchor == end && !latest {
			continue
		}
		line, e := json.Marshal(map[string]any{"kind": "collaboration", "ts": run.CreatedAt, "run_id": run.ID, "text": run.Request.Text})
		if e != nil {
			return nil, e
		}
		at[anchor] = append(at[anchor], line)
	}
	out := []json.RawMessage{}
	for i := start; i <= end; i++ {
		out = append(out, at[i]...)
		if i < end && historySent(entries[i]) {
			out = append(out, entries[i].raw)
		}
	}
	return out, nil
}

func (s *Server) hasThreadCollaborations(sess store.Session, thread string) bool {
	if s.store == nil {
		return false
	}
	u, ok := s.store.GetUser(sess.User)
	if !ok {
		return false
	}
	runs, err := s.store.CollaborationThreadRuns(u, sess.ID, thread)
	return err != nil || len(runs) > 0
}

// Bounded directory suggestions only. Git preflight remains authoritative and
// chooses no repository automatically when multiple candidates exist.
func (s *Server) handleCollaborationRepositories(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	root, err := s.openDataDir(s.workspaceDir(sess))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	defer root.Close()
	paths := []string{}
	hasGit := func(dir string) bool {
		sub, e := root.Sub(dir)
		if e != nil {
			return false
		}
		defer sub.Close()
		info, e := sub.Lstat(".git")
		return e == nil && info.IsDir()
	}
	if hasGit(".") {
		paths = append(paths, ".")
	}
	entries, err := root.ReadDirLimit(".", 200)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && hasGit(entry.Name()) {
				paths = append(paths, entry.Name())
			}
		}
	}
	writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(reqUser(r)), "repositories": paths, "truncated": err != nil})
}
