package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"time"

	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"agentbox/internal/syncproto"
)

// This owner-scoped inventory is deliberately independent of project mappings
// and local client state. A device is an original journal label, not a second
// authentication factor; the workspace owner explicitly selects it for action.
type recoveryCursor struct{ Server, Workspace, Device, After string }

func (s *Server) requireRecoveryIdentity(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := s.clientIdentity()
	if err != nil || id == "" || r.Header.Get("X-Agentbox-Server-ID") != id {
		writeErr(w, http.StatusConflict, "恢复管理必须绑定已确认的服务器身份")
		return "", false
	}
	return id, true
}

func (s *Server) handleClientRecoveryOperations(w http.ResponseWriter, r *http.Request, sess store.Session) {
	serverID, ok := s.requireRecoveryIdentity(w, r)
	if !ok {
		return
	}
	device, cursor := r.URL.Query().Get("device"), r.URL.Query().Get("cursor")
	if device != "" && (syncproto.RecoveryInspectRetireRequest{Device: device, Confirmation: syncproto.HashBytes(nil)}).Validate() != nil {
		writeErr(w, 400, "设备范围无效")
		return
	}
	position := recoveryCursor{Server: serverID, Workspace: sess.ID, Device: device}
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		var decoded recoveryCursor
		if len(cursor) > 4096 || err != nil || json.Unmarshal(raw, &decoded) != nil || decoded.Server != serverID || decoded.Workspace != sess.ID || decoded.Device != device || !syncproto.ValidOperationID(decoded.After) {
			writeErr(w, 400, "恢复列表游标无效")
			return
		}
		position = decoded
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	page := syncproto.RecoveryOperationsPage{ServerID: serverID, Workspace: sess.ID, Device: device, Items: []syncproto.RecoveryListItem{}}
	err := s.workspaces().WithSession(ctx, sess.ID, func(current store.Session) error {
		session, err := s.openDataDir(s.sessionDir(current))
		if err != nil {
			return err
		}
		defer session.Close()
		journal, err := session.Sub("client-sync")
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer journal.Close()
		ids, err := clientRecoveryIDs(ctx, journal)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if id <= position.After {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			operation, err := journal.Sub(id)
			if err != nil {
				return err
			}
			record, readErr := loadClientMutationRecord(operation, id)
			operation.Close()
			item := syncproto.RecoveryListItem{ID: id}
			if readErr != nil {
				switch {
				case os.IsNotExist(readErr):
					item.Issue = "missing_record"
				case errors.Is(readErr, syncproto.ErrInvalid):
					item.Issue = "invalid_record"
				default:
					return readErr
				}
				if device != "" {
					continue
				}
			} else {
				if device != "" && record.Request.Device != device {
					continue
				}
				status := clientOperationStatus(record, serverID, current.ID)
				item.Status = &status
			}
			if len(page.Items) == syncproto.RecoveryPageLimit {
				position.After = page.Items[len(page.Items)-1].ID
				raw, _ := json.Marshal(position)
				page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
				break
			}
			page.Items = append(page.Items, item)
		}
		return ctx.Err()
	})
	if err != nil {
		writeMutationError(w, err, syncproto.MutationResult{})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, page)
}

func clientRecoveryIDs(ctx context.Context, journal *safefs.Root) ([]string, error) {
	f, err := journal.FS().Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	directory, ok := f.(fs.ReadDirFile)
	if !ok {
		return nil, syncproto.ErrInvalid
	}
	ids := []string{}
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(ids) >= clientJournalLimit+clientReceiptLimit {
				return nil, syncproto.ErrLimit
			}
			if !entry.IsDir() || !syncproto.ValidOperationID(entry.Name()) {
				return nil, syncproto.ErrInvalid
			}
			ids = append(ids, entry.Name())
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func (s *Server) handleClientRecoveryInspect(w http.ResponseWriter, r *http.Request, sess store.Session) {
	s.handleClientRecoveryInspection(w, r, sess, false)
}
func (s *Server) handleClientRecoveryInspectRetire(w http.ResponseWriter, r *http.Request, sess store.Session) {
	s.handleClientRecoveryInspection(w, r, sess, true)
}

func (s *Server) handleClientRecoveryInspection(w http.ResponseWriter, r *http.Request, sess store.Session, retire bool) {
	serverID, ok := s.requireRecoveryIdentity(w, r)
	if !ok {
		return
	}
	id := r.PathValue("operation")
	request := syncproto.RecoveryInspectRetireRequest{Device: r.URL.Query().Get("device"), Confirmation: syncproto.HashBytes(nil)}
	if !syncproto.ValidOperationID(id) || retire && decodeClientRequest(w, r, &request) != nil || request.Validate() != nil {
		writeErr(w, 400, "恢复核对参数无效")
		return
	}
	s.clientManifestOnce.Do(func() { s.clientManifestSlots = make(chan struct{}, 2) })
	select {
	case s.clientManifestSlots <- struct{}{}:
		defer func() { <-s.clientManifestSlots }()
	default:
		writeErr(w, 429, "同步读取繁忙，请稍后重试")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var review syncproto.RecoveryOperationReview
	err := s.workspaces().WithSession(ctx, sess.ID, func(current store.Session) (err error) {
		defer func() {
			if retire && err != nil {
				s.forgetClientInventory(current.ID)
			}
		}()
		session, err := s.openDataDir(s.sessionDir(current))
		if err != nil {
			return err
		}
		defer session.Close()
		journal, err := session.Sub("client-sync")
		if os.IsNotExist(err) {
			return store.ErrClientMissing
		}
		if err != nil {
			return err
		}
		defer journal.Close()
		operation, err := journal.Sub(id)
		if os.IsNotExist(err) {
			return store.ErrClientMissing
		}
		if err != nil {
			return err
		}
		defer operation.Close()
		record, err := loadClientMutationRecord(operation, id)
		if err != nil {
			return err
		}
		if record.Request.Device != request.Device {
			return syncproto.ErrChanged
		}
		review, err = s.inspectClientRecovery(ctx, current, operation, record, serverID)
		if err != nil {
			return err
		}
		if !retire {
			return nil
		}
		// A previously persisted authorization resumes exactly the same operation
		// after cancellation/process loss, even if the user edited its current file.
		resume := record.Retirement != "" && record.InspectionConfirmation == request.Confirmation
		if record.Result.Status != "applied" || review.LeaseActive || !resume && review.Digest != request.Confirmation {
			return syncproto.ErrChanged
		}
		if record.Retirement == "retired" {
			return nil
		}
		inventory, err := s.clientInventory(ctx, current.ID, journal)
		if err != nil {
			return err
		}
		before, err := inspectClientInventoryEntry(journal, id)
		if err != nil {
			return err
		}
		if record.Retirement == "" && inventory.storage.RetainedReceipts >= clientReceiptLimit {
			return syncproto.ErrLimit
		}
		if !resume {
			record.InspectionConfirmation = request.Confirmation
			if record.Retirement != "" {
				if err = saveClientMutation(operation, record); err != nil {
					return err
				}
			}
		}
		if err = retireClientOperation(ctx, operation, &record, nil); err != nil {
			return err
		}
		after, err := inspectClientInventoryEntry(journal, id)
		if err != nil {
			return err
		}
		inventory.adjust(before, after)
		review.Status = clientOperationStatus(record, serverID, current.ID)
		review.RecoveryState = "retired"
		review.CanRetire = false
		return nil
	})
	if err != nil {
		writeMutationError(w, err, syncproto.MutationResult{})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, review)
}

func (s *Server) inspectClientRecovery(ctx context.Context, sess store.Session, operation *safefs.Root, record clientMutationRecord, server string) (syncproto.RecoveryOperationReview, error) {
	r := syncproto.RecoveryOperationReview{ServerID: server, Workspace: sess.ID, Status: clientOperationStatus(record, server, sess.ID), Comparison: "unavailable", RecoveryState: "none", LeaseActive: s.syncLeases().WorkspaceActive(sess.ID)}
	if record.Retirement != "" {
		r.RecoveryState = record.Retirement
	} else if record.Request.Before != nil && record.Request.Before.Kind == "file" {
		info, err := operation.Lstat("before")
		if os.IsNotExist(err) {
			r.RecoveryState = "missing"
		} else if err != nil {
			return r, err
		} else {
			actual, err := copyManifestFile(ctx, operation, "before", info, io.Discard)
			if err != nil || actual.Hash != record.Request.Before.Hash || actual.Size != record.Request.Before.Size {
				r.RecoveryState = "corrupt"
			} else {
				r.RecoveryState = "available"
			}
		}
	}
	project, err := s.store.ClientProject(sess.ID, record.Request.Project)
	if errors.Is(err, store.ErrClientMissing) {
		r.Comparison = "project_missing"
	} else if err != nil {
		return r, err
	} else {
		r.ProjectPath = project.Path
		r.ProjectRevision = project.Revision
		if project.Revision != record.Request.Revision {
			r.Comparison = "project_changed"
		} else {
			current, readErr := s.readClientRecoveryCurrent(ctx, sess, project.Path, record.Request.Path)
			if readErr == nil {
				r.Current = current
				r.Comparison = "changed"
				if recoveryEntriesEqual(current, record.Request.After) {
					r.Comparison = "matches_after"
				} else if recoveryEntriesEqual(current, record.Request.Before) {
					r.Comparison = "matches_before"
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	r.CanRetire = record.Result.Status == "applied" && record.Retirement != "retired" && !r.LeaseActive && r.RecoveryState != "missing" && r.RecoveryState != "corrupt"
	r.Digest = r.Confirmation()
	return r, nil
}

func recoveryEntriesEqual(a, b *syncproto.Entry) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (s *Server) readClientRecoveryCurrent(ctx context.Context, sess store.Session, projectPath, filename string) (*syncproto.Entry, error) {
	if !store.ClientProjectPath(projectPath) {
		return nil, syncproto.ErrInvalid
	}
	workspace, err := s.openDataDir(s.workspaceDir(sess))
	if err != nil {
		return nil, err
	}
	defer workspace.Close()
	root, err := workspace.Sub(projectPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	before, err := root.Lstat(filename)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if before.IsDir() {
		return &syncproto.Entry{Kind: "directory"}, nil
	}
	entry, err := copyManifestFile(ctx, root, filename, before, io.Discard)
	if err != nil {
		return nil, err
	}
	return &entry, nil
}
