package server

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"agentbox/internal/store"
	"agentbox/internal/syncproto"
)

// Downloads are immutable, bounded snapshots: never send headers or bytes until
// the expected content and mapping have been checked. Share the two global scan
// slots, retaining the slot through response delivery (at most 128 MiB of data).
// No lease is required for this read-only operation.
func (s *Server) handleClientFile(w http.ResponseWriter, r *http.Request, sess store.Session) {
	q := r.URL.Query()
	revision, revisionErr := strconv.ParseInt(q.Get("project_revision"), 10, 64)
	size, sizeErr := strconv.ParseInt(q.Get("size"), 10, 64)
	executable := q.Get("executable")
	request := syncproto.FileRequest{Project: q.Get("project"), Revision: revision, RulesHash: q.Get("rules_hash"), Path: q.Get("path"), Expected: syncproto.Entry{Kind: "file", Hash: q.Get("hash"), Size: size, Executable: executable == "true"}}
	if revisionErr != nil || sizeErr != nil || (executable != "true" && executable != "false") || request.Validate() != nil || !store.ValidClientResourceID(request.Project) {
		writeErr(w, http.StatusBadRequest, "文件下载条件无效")
		return
	}
	s.clientManifestOnce.Do(func() { s.clientManifestSlots = make(chan struct{}, 2) })
	select {
	case s.clientManifestSlots <- struct{}{}:
		defer func() { <-s.clientManifestSlots }()
	default:
		writeErr(w, http.StatusTooManyRequests, "同步读取繁忙，请稍后重试")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	snapshot := boundedSnapshot{remaining: request.Expected.Size}
	err := s.workspaces().WithSession(ctx, sess.ID, func(current store.Session) error {
		project, err := s.store.ClientProject(current.ID, request.Project)
		if err != nil {
			return err
		}
		if project.Revision != request.Revision {
			return syncproto.ErrChanged
		}
		if !store.ClientProjectPath(project.Path) {
			return syncproto.ErrInvalid
		}
		workspace, err := s.openDataDir(s.workspaceDir(current))
		if err != nil {
			return err
		}
		defer workspace.Close()
		root, err := workspace.Sub(project.Path)
		if err != nil {
			return err
		}
		defer root.Close()
		identity, err := root.Lstat(".")
		if err != nil {
			return err
		}
		rulesText, present, err := clientIgnoreFile(ctx, root)
		if err != nil {
			return err
		}
		rules, err := syncproto.ParseRules(string(rulesText))
		if err != nil {
			return err
		}
		if rules.Hash() != request.RulesHash {
			return syncproto.ErrChanged
		}
		if rules.Ignored(request.Path) {
			return syncproto.ErrInvalid
		}
		before, err := root.Lstat(request.Path)
		if err != nil {
			return err
		}
		if !before.Mode().IsRegular() {
			return syncproto.ErrInvalid
		}
		if before.Size() != request.Expected.Size {
			return syncproto.ErrChanged
		}
		snapshot.buffer.Grow(int(request.Expected.Size))
		actual, err := copyManifestFile(ctx, root, request.Path, before, &snapshot)
		if err != nil {
			return err
		}
		if actual != request.Expected {
			return syncproto.ErrChanged
		}
		afterRules, afterPresent, err := clientIgnoreFile(ctx, root)
		if err != nil {
			return err
		}
		if present != afterPresent || !bytes.Equal(rulesText, afterRules) {
			return syncproto.ErrChanged
		}
		fresh, err := workspace.Sub(project.Path)
		if err != nil {
			return err
		}
		defer fresh.Close()
		after, err := fresh.Lstat(".")
		if err != nil {
			return err
		}
		if !os.SameFile(identity, after) {
			return syncproto.ErrChanged
		}
		return ctx.Err()
	})
	if err != nil {
		writeManifestError(w, err)
		return
	}
	// The workspace lock is released before a slow network peer can stall delivery.
	// These are bytes already verified, not a second open of a mutable pathname.
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(60 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(request.Expected.Size, 10))
	w.Header().Set("ETag", `"`+request.Expected.Hash+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(snapshot.buffer.Bytes())
}

type boundedSnapshot struct {
	buffer    bytes.Buffer
	remaining int64
}

func (b *boundedSnapshot) Write(p []byte) (int, error) {
	if int64(len(p)) > b.remaining {
		return 0, syncproto.ErrChanged
	}
	n, err := b.buffer.Write(p)
	b.remaining -= int64(n)
	return n, err
}
