package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"runtime"
	"strconv"
	"time"

	"agentbox/internal/dockerx"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"agentbox/internal/syncproto"
)

const clientJournalLimit = 1000
const clientRecoveryBytes int64 = 256 << 20

var errClientUncertain = errors.New("sync operation requires reconciliation")

type clientMutationRecord struct {
	Digest                 string                   `json:"digest"`
	Request                syncproto.Mutation       `json:"request"`
	Result                 syncproto.MutationResult `json:"result"`
	Created                time.Time                `json:"created_at"`
	Retirement             string                   `json:"retirement,omitempty"`
	InspectionConfirmation string                   `json:"inspection_confirmation,omitempty"`
}

func (s *Server) handleClientMutation(w http.ResponseWriter, r *http.Request, sess store.Session) {
	encoded := r.Header.Get("X-Agentbox-Sync-Request")
	if len(encoded) > 16<<10 {
		writeErr(w, 400, "同步操作参数过大")
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	var request syncproto.Mutation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err != nil || decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Validate() != nil || !store.ValidClientResourceID(request.Project) {
		writeErr(w, 400, "同步操作参数无效")
		return
	}
	// Reject stale writers before reading their upload, then recheck under the
	// workspace lock after the network read. Uploads never prevent lease renewal.
	token := r.Header.Get("X-Agentbox-Sync-Lease")
	err = s.workspaces().WithSession(r.Context(), sess.ID, func(current store.Session) error {
		p, err := s.store.ClientProject(current.ID, request.Project)
		if err != nil {
			return err
		}
		if p.Revision != request.Revision {
			return syncproto.ErrChanged
		}
		_, err = s.syncLeases().Check(current.ID, p.ID, p.Path, request.Device, token, request.Generation)
		return err
	})
	if err != nil {
		writeMutationError(w, err, syncproto.MutationResult{})
		return
	}
	s.clientManifestOnce.Do(func() { s.clientManifestSlots = make(chan struct{}, 2) })
	select {
	case s.clientManifestSlots <- struct{}{}:
		defer func() { <-s.clientManifestSlots }()
	default:
		writeErr(w, 429, "同步传输繁忙，请稍后重试")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(60 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	size := int64(0)
	if request.After != nil {
		size = request.After.Size
	}
	payload := boundedSnapshot{remaining: size}
	payload.buffer.Grow(int(size))
	if _, err = io.Copy(&payload, io.LimitReader(manifestReader{ctx, r.Body}, size+1)); err != nil || int64(payload.buffer.Len()) != size || request.Kind == "replace" && syncproto.HashBytes(payload.buffer.Bytes()) != request.After.Hash {
		writeErr(w, 400, "上传长度或哈希不匹配，文件未修改")
		return
	}
	var result syncproto.MutationResult
	err = s.workspaces().WithSession(ctx, sess.ID, func(current store.Session) error {
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
		check := func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := s.syncLeases().Check(current.ID, project.ID, project.Path, request.Device, token, request.Generation); err != nil {
				return err
			}
			raw, _, err := clientIgnoreFile(ctx, root)
			if err != nil {
				return err
			}
			rules, err := syncproto.ParseRules(string(raw))
			if err != nil {
				return err
			}
			if rules.Hash() != request.RulesHash {
				return syncproto.ErrChanged
			}
			if rules.Ignored(request.Path) {
				return syncproto.ErrInvalid
			}
			fresh, err := workspace.Sub(project.Path)
			if err != nil {
				return err
			}
			defer fresh.Close()
			now, err := fresh.Lstat(".")
			if err != nil {
				return err
			}
			if !os.SameFile(identity, now) {
				return syncproto.ErrChanged
			}
			_, err = s.syncLeases().Check(current.ID, project.ID, project.Path, request.Device, token, request.Generation)
			return err
		}
		if err = check(); err != nil {
			return err
		}
		session, err := s.openDataDir(s.sessionDir(current))
		if err != nil {
			return err
		}
		defer session.Close()
		if err = session.Mkdir("client-sync", 0700); err != nil && !os.IsExist(err) {
			return err
		}
		if err = session.Sync(); err != nil {
			return err
		}
		journal, err := session.Sub("client-sync")
		if err != nil {
			return err
		}
		defer journal.Close()
		inventory, err := s.clientInventory(ctx, current.ID, journal)
		if err != nil {
			return err
		}
		result, err = applyClientMutation(ctx, root, journal, request, bytes.NewReader(payload.buffer.Bytes()), check, inventory)
		if err != nil && inventory.dirty {
			s.forgetClientInventory(current.ID)
		}
		return err
	})
	if err != nil {
		writeMutationError(w, err, result)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, result)
}

func writeMutationError(w http.ResponseWriter, err error, result syncproto.MutationResult) {
	w.Header().Set("Cache-Control", "no-store")
	if errors.Is(err, errClientUncertain) {
		writeJSON(w, 409, map[string]any{"error": "操作结果未确认，请核对清单和恢复副本；不能直接重放", "operation": result})
		return
	}
	if errors.Is(err, syncproto.ErrLeaseExpired) {
		writeErr(w, 409, "同步租约已失效，请重新获取并核对计划")
		return
	}
	writeManifestError(w, err)
}

func saveClientMutation(dir *safefs.Root, record clientMutationRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err = dir.WriteFile("record.json", raw, safefs.WriteOptions{Mode: 0600, MaxBytes: 16 << 10}); err != nil {
		return err
	}
	return dir.Sync()
}

// journal is service-owned and outside workspace/home container mounts. This
// durable intent log prevents blind replay across a crash after rename/unlink.
// It does not implement an atomic filesystem+database transaction or lock CLI
// writes; uncertainty is surfaced and both previous content and intent survive.
func applyClientMutation(ctx context.Context, root, journal *safefs.Root, request syncproto.Mutation, source io.Reader, check func() error, inventories ...*clientJournalInventory) (result syncproto.MutationResult, err error) {
	digest, err := request.Digest()
	if err != nil {
		return result, err
	}
	if existing, openErr := journal.Sub(request.ID); openErr == nil {
		defer existing.Close()
		raw, err := existing.ReadAll("record.json", 16<<10)
		if err != nil {
			return syncproto.MutationResult{ID: request.ID, Status: "uncertain", Replayed: true}, errClientUncertain
		}
		var record clientMutationRecord
		if json.Unmarshal(raw, &record) != nil || record.Result.ID != request.ID || record.Digest != digest {
			return result, syncproto.ErrChanged
		}
		result = record.Result
		result.Replayed = true
		if result.Status != "applied" {
			result.Status = "uncertain"
			return result, errClientUncertain
		}
		return result, nil
	} else if !os.IsNotExist(openErr) {
		return result, openErr
	}
	parent, err := root.Sub(path.Dir(request.Path))
	if err != nil {
		return result, err
	}
	defer parent.Close()
	identity, err := parent.Lstat(".")
	if err != nil {
		return result, err
	}
	leaf := path.Base(request.Path)
	var directoryIdentity os.FileInfo
	if request.Kind == "rmdir" {
		directoryIdentity, err = parent.Lstat(leaf)
		if err != nil {
			return result, err
		}
	}
	verify := func() error {
		if err := check(); err != nil {
			return err
		}
		fresh, err := root.Sub(path.Dir(request.Path))
		if err != nil {
			return err
		}
		defer fresh.Close()
		actual, err := fresh.Lstat(".")
		if err != nil {
			return err
		}
		if !os.SameFile(identity, actual) {
			return syncproto.ErrChanged
		}
		if err := checkClientExpected(ctx, parent, leaf, request.Before); err != nil {
			return err
		}
		if request.Kind == "rmdir" {
			current, err := parent.Lstat(leaf)
			if err != nil {
				return err
			}
			if !os.SameFile(directoryIdentity, current) {
				return syncproto.ErrChanged
			}
			return clientDirectoryEmpty(parent, leaf)
		}
		return nil
	}
	if err = verify(); err != nil {
		return result, err
	}
	var inventory *clientJournalInventory
	if len(inventories) != 0 {
		inventory = inventories[0]
	} else {
		inventory, err = scanClientInventory(ctx, journal)
		if err != nil {
			return result, err
		}
	}
	if inventory.storage.ActiveOperations >= clientJournalLimit || request.Before != nil && request.Before.Kind == "file" && inventory.storage.RecoveryBytes > clientRecoveryBytes-request.Before.Size {
		return result, syncproto.ErrLimit
	}
	inventory.dirty = true
	if err = journal.Mkdir(request.ID, 0700); err != nil {
		return result, err
	}
	if err = journal.Sync(); err != nil {
		return result, err
	}
	operation, err := journal.Sub(request.ID)
	if err != nil {
		return result, err
	}
	defer operation.Close()
	record := clientMutationRecord{Digest: digest, Request: request, Result: syncproto.MutationResult{ID: request.ID, Status: "uncertain"}, Created: time.Now().UTC()}
	record.Request.Generation = "" // no lease material persisted
	result = record.Result
	if err = saveClientMutation(operation, record); err != nil {
		return result, err
	}
	// From this point a retry can only inspect a durable result. Even a failure
	// before publication conservatively requires a fresh scan/new operation ID.
	defer func() {
		if err != nil {
			err = errClientUncertain
			result = record.Result
		}
	}()
	staged := ".agentbox-sync-tmp-" + rand.Text()
	if request.Kind == "replace" {
		mode := os.FileMode(0644)
		if request.Before != nil {
			info, e := parent.Lstat(leaf)
			if e != nil {
				return result, e
			}
			mode = info.Mode().Perm() &^ 0111
		}
		if request.After.Executable {
			mode |= 0111
		}
		_, err = parent.WriteAtomic(staged, source, safefs.WriteOptions{Mode: mode, MaxBytes: request.After.Size, Limit: true, Chown: true, UID: dockerx.AgentUID, GID: dockerx.AgentGID, BestEffortChown: runtime.GOOS == "darwin" && os.Geteuid() != 0})
		if err != nil {
			return result, err
		}
		defer parent.Remove(staged)
		if err = checkClientExpected(ctx, parent, staged, request.After); err != nil {
			return result, err
		}
	}
	if request.Kind == "mkdir" {
		if err = parent.Mkdir(staged, 0755); err != nil {
			return result, err
		}
		defer parent.RemoveDirectory(staged)
		if err = parent.Chown(staged, dockerx.AgentUID, dockerx.AgentGID); err != nil && !(runtime.GOOS == "darwin" && os.Geteuid() != 0) {
			return result, err
		}
		if err = parent.Sync(); err != nil {
			return result, err
		}
	}
	if request.Before != nil && request.Before.Kind == "file" {
		info, e := parent.Lstat(leaf)
		if e != nil {
			return result, e
		}
		snapshot := boundedSnapshot{remaining: request.Before.Size}
		snapshot.buffer.Grow(int(request.Before.Size))
		value, e := copyManifestFile(ctx, parent, leaf, info, &snapshot)
		if e != nil {
			return result, e
		}
		if value != *request.Before {
			return result, syncproto.ErrChanged
		}
		if _, err = operation.WriteFile("before", snapshot.buffer.Bytes(), safefs.WriteOptions{Mode: 0600, MaxBytes: request.Before.Size, Limit: true}); err != nil {
			return result, err
		}
		if err = operation.Sync(); err != nil {
			return result, err
		}
		record.Result.Recovery = true
		if err = saveClientMutation(operation, record); err != nil {
			return result, err
		}
	}
	if request.Kind == "replace" {
		if err = checkClientExpected(ctx, parent, staged, request.After); err != nil {
			return result, err
		}
	}
	if request.Kind == "mkdir" {
		if err = checkClientExpected(ctx, parent, staged, request.After); err != nil {
			return result, err
		}
		if err = clientDirectoryEmpty(parent, staged); err != nil {
			return result, err
		}
	}
	if err = verify(); err != nil {
		return result, err
	}
	if request.Kind == "replace" || request.Kind == "mkdir" {
		// Fencing is checked immediately before publication as well as before hash
		// reads, which can take longer than a lease's remaining lifetime.
		if err = check(); err != nil {
			return result, err
		}
		err = parent.RenameTo(staged, parent, leaf, request.Before != nil)
	} else {
		if err = check(); err != nil {
			return result, err
		}
		if request.Kind == "rmdir" {
			err = parent.RemoveDirectory(leaf)
		} else {
			err = parent.Remove(leaf)
		}
	}
	if err != nil {
		return result, err
	}
	if err = parent.Sync(); err != nil {
		return result, err
	}
	if err = checkClientExpected(ctx, parent, leaf, request.After); err != nil {
		return result, err
	}
	record.Result.Status = "applied"
	if err = saveClientMutation(operation, record); err != nil {
		record.Result.Status = "uncertain"
		return result, err
	}
	entry, err := inspectClientInventoryEntry(journal, request.ID)
	if err != nil {
		return result, err
	}
	inventory.adjust(clientInventoryEntry{}, entry)
	inventory.dirty = false
	return record.Result, nil
}

func checkClientExpected(ctx context.Context, root *safefs.Root, name string, expected *syncproto.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if expected == nil {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return syncproto.ErrChanged
	}
	if err != nil {
		return err
	}
	if expected.Kind == "directory" {
		if !info.IsDir() {
			return syncproto.ErrChanged
		}
		dir, err := root.Sub(name)
		if err != nil {
			return err
		}
		return dir.Close()
	}
	if !info.Mode().IsRegular() {
		return syncproto.ErrInvalid
	}
	actual, err := hashManifestFile(ctx, root, name, info)
	if err != nil {
		return err
	}
	if actual != *expected {
		return syncproto.ErrChanged
	}
	return nil
}

// Status and recovery remain readable after lease expiry or project removal.
// They require the owning workspace and never create directories or start Docker.
func (s *Server) handleClientOperation(w http.ResponseWriter, r *http.Request, sess store.Session) {
	id := r.PathValue("operation")
	if !syncproto.ValidOperationID(id) {
		writeErr(w, 400, "操作 ID 无效")
		return
	}
	if value := r.PathValue("recovery"); value != "" && value != "before" {
		writeErr(w, 404, "恢复资源不存在")
		return
	}
	recovery := r.PathValue("recovery") == "before"
	if recovery {
		s.clientManifestOnce.Do(func() { s.clientManifestSlots = make(chan struct{}, 2) })
		select {
		case s.clientManifestSlots <- struct{}{}:
			defer func() { <-s.clientManifestSlots }()
		default:
			writeErr(w, 429, "同步传输繁忙，请稍后重试")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var record clientMutationRecord
	var snapshot boundedSnapshot
	err := s.workspaces().WithSession(ctx, sess.ID, func(current store.Session) error {
		session, err := s.openDataDir(s.sessionDir(current))
		if err != nil {
			return err
		}
		defer session.Close()
		operation, err := session.Sub("client-sync/" + id)
		if os.IsNotExist(err) {
			return store.ErrClientMissing
		}
		if err != nil {
			return err
		}
		defer operation.Close()
		record, err = loadClientMutationRecord(operation, id)
		if err != nil {
			return err
		}
		if !recovery {
			return nil
		}
		if record.Retirement != "" || record.Request.Before == nil || record.Request.Before.Kind != "file" {
			return store.ErrClientMissing
		}
		before, err := operation.Lstat("before")
		if os.IsNotExist(err) {
			return store.ErrClientMissing
		}
		if err != nil {
			return err
		}
		snapshot.remaining = record.Request.Before.Size
		if snapshot.remaining < 0 || snapshot.remaining > syncproto.DefaultMaxFileBytes {
			return syncproto.ErrInvalid
		}
		snapshot.buffer.Grow(int(snapshot.remaining))
		actual, err := copyManifestFile(ctx, operation, "before", before, &snapshot)
		if err != nil {
			return err
		}
		// Recovery files are intentionally private/non-executable; original mode
		// metadata stays in the request. Validate the saved bytes independently.
		if actual.Hash != record.Request.Before.Hash || actual.Size != record.Request.Before.Size {
			return syncproto.ErrChanged
		}
		return nil
	})
	if err != nil {
		writeMutationError(w, err, syncproto.MutationResult{})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !recovery {
		serverID, err := s.clientIdentity()
		if err != nil {
			writeMutationError(w, err, syncproto.MutationResult{})
			return
		}
		writeJSON(w, 200, struct {
			syncproto.OperationStatus
			Created time.Time `json:"created_at"`
		}{clientOperationStatus(record, serverID, sess.ID), record.Created})
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(60 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(record.Request.Before.Size, 10))
	w.Header().Set("ETag", `"`+record.Request.Before.Hash+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="agentbox-recovery-`+id+`"`)
	w.WriteHeader(200)
	_, _ = w.Write(snapshot.buffer.Bytes())
}

func clientDirectoryEmpty(root *safefs.Root, name string) error {
	f, err := root.FS().Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	dir, ok := f.(fs.ReadDirFile)
	if !ok {
		return syncproto.ErrInvalid
	}
	entries, err := dir.ReadDir(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) != 0 {
		return syncproto.ErrChanged
	}
	return nil
}
