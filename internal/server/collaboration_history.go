package server

import (
	"agentbox/internal/chat"
	"agentbox/internal/collaboration"
	"agentbox/internal/store"
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
)

// Transcript access stays on the task route, never opens ordinary participant
// session APIs. The byte cursor only advances across complete persisted lines.
func (s *Server) handleCollaborationHistory(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	if run.State == store.CollabDeleted {
		writeCollaborationError(w, r, store.ErrChatGone)
		return
	}
	step, err := s.store.CollaborationStep(r.PathValue("step"))
	if err != nil || step.RunID != run.ID {
		writeCollaborationError(w, r, collaboration.ErrInvalid)
		return
	}
	participants, err := s.store.CollaborationParticipants(u, run.ID)
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	session := ""
	for _, p := range participants {
		if p.Role == step.Role && p.Generation == step.ParticipantGeneration {
			session = p.SessionID
		}
	}
	if session == "" {
		writeCollaborationError(w, r, collaboration.ErrInvalid)
		return
	}
	offset := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		offset, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || offset < 0 {
			writeCollaborationError(w, r, collaboration.ErrInvalid)
			return
		}
	}
	root, err := s.openDataDir(filepath.Join(s.cfg.DataDir, "users", u.Name, "sessions", session, "chats"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	defer root.Close()
	file, err := root.OpenFile(run.ID + ".jsonl")
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || offset > info.Size() {
		writeCollaborationError(w, r, collaboration.ErrInvalid)
		return
	}
	if offset > 0 {
		var prior [1]byte
		if _, err = file.ReadAt(prior[:], offset-1); err != nil || prior[0] != '\n' {
			writeCollaborationError(w, r, collaboration.ErrInvalid)
			return
		}
	}
	if _, err = file.Seek(offset, 0); err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	reader := bufio.NewReaderSize(io.LimitReader(file, 128<<20), 64<<10)
	entries := []chat.Entry{}
	size := 0
	truncated := false
	for len(entries) < 100 && size < 256<<10 {
		var line []byte
		consumed := 0
		complete := false
		for {
			part, e := reader.ReadSlice('\n')
			consumed += len(part)
			if consumed <= 256<<10 {
				line = append(line, part...)
			}
			if e == nil {
				complete = true
				break
			}
			if errors.Is(e, bufio.ErrBufferFull) {
				continue
			}
			if !errors.Is(e, io.EOF) {
				writeCollaborationError(w, r, e)
				return
			}
			break
		}
		if !complete {
			break
		}
		var entry chat.Entry
		if consumed > 256<<10 {
			entry = chat.Entry{Kind: "status", State: "truncated", Text: "Oversized recorded event omitted from this preview."}
			truncated = true
		} else if json.Unmarshal(line, &entry) != nil {
			writeCollaborationError(w, r, collaboration.ErrInvalid)
			return
		}
		entries = append(entries, entry)
		offset += int64(consumed)
		size += consumed
	}

	writeJSON(w, 200, map[string]any{"version": 1, "scope": s.collaborationScope(u), "run_id": run.ID, "step_id": step.ID, "role": step.Role, "generation": step.ParticipantGeneration, "entries": entries, "next": offset, "has_more": offset < info.Size(), "truncated": truncated})
}
