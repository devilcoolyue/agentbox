package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"time"

	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"agentbox/internal/syncproto"
)

const clientReceiptLimit = 100000

// All inventory access is under the workspace lifecycle lock. The map mutex
// only protects different workspaces. The data directory is process-exclusive
// and this journal is outside container mounts; inode checks detect replacement.
// No replay decision ever uses this capacity cache: the on-disk ID directory is
// always authoritative. Failed writes invalidate the cache before unlocking.
type clientJournalInventory struct {
	identity os.FileInfo
	storage  syncproto.RecoveryStorage
	dirty    bool
}

func emptyClientStorage() syncproto.RecoveryStorage {
	return syncproto.RecoveryStorage{OperationLimit: clientJournalLimit, ReceiptLimit: clientReceiptLimit, RecoveryByteLimit: clientRecoveryBytes}
}

func (s *Server) forgetClientInventory(id string) {
	s.clientInventoryMu.Lock()
	delete(s.clientInventories, id)
	s.clientInventoryMu.Unlock()
}

func (s *Server) clientInventory(ctx context.Context, id string, journal *safefs.Root) (*clientJournalInventory, error) {
	info, err := journal.Lstat(".")
	if err != nil {
		s.forgetClientInventory(id)
		return nil, err
	}
	s.clientInventoryMu.Lock()
	cached := s.clientInventories[id]
	s.clientInventoryMu.Unlock()
	if cached != nil && !cached.dirty && os.SameFile(cached.identity, info) {
		return cached, nil
	}
	inventory, err := scanClientInventory(ctx, journal)
	if err != nil {
		s.forgetClientInventory(id)
		return nil, err
	}
	inventory.identity = info
	s.clientInventoryMu.Lock()
	if s.clientInventories == nil || len(s.clientInventories) >= 256 {
		// Dropping a cache cannot lose receipts or change operation eligibility.
		s.clientInventories = make(map[string]*clientJournalInventory)
	}
	s.clientInventories[id] = inventory
	s.clientInventoryMu.Unlock()
	return inventory, nil
}

type clientInventoryEntry struct {
	active, retained   int
	recovery, metadata int64
}

func (i *clientJournalInventory) adjust(before, after clientInventoryEntry) {
	i.storage.ActiveOperations += after.active - before.active
	i.storage.RetainedReceipts += after.retained - before.retained
	i.storage.RecoveryBytes += after.recovery - before.recovery
	i.storage.MetadataBytes += after.metadata - before.metadata
}

// Missing intent files can be left by a crash after mkdir. They occupy a live
// slot forever until explicitly reconciled, and are never mistaken for retired.
func inspectClientInventoryEntry(journal *safefs.Root, id string) (clientInventoryEntry, error) {
	result := clientInventoryEntry{active: 1}
	operation, err := journal.Sub(id)
	if err != nil {
		return result, err
	}
	defer operation.Close()
	info, err := operation.Lstat("record.json")
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() > 16<<10 {
			return result, syncproto.ErrInvalid
		}
		result.metadata = info.Size()
		raw, err := operation.ReadAll("record.json", 16<<10)
		if err != nil {
			return result, err
		}
		var record clientMutationRecord
		if json.Unmarshal(raw, &record) != nil {
			return result, syncproto.ErrInvalid
		}
		if record.Retirement != "" {
			if validateClientMutationRecord(record, id) != nil {
				return result, syncproto.ErrInvalid
			}
			result.retained = 1 // retiring reserves its permanent slot before unlink
			if record.Retirement == "retired" {
				result.active = 0
			}
		}
	} else if !os.IsNotExist(err) {
		return result, err
	}
	info, err = operation.Lstat("before")
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() < 0 || result.active == 0 {
			return result, syncproto.ErrInvalid
		}
		result.recovery = info.Size()
	} else if !os.IsNotExist(err) {
		return result, err
	}
	return result, nil
}

func scanClientInventory(ctx context.Context, journal *safefs.Root) (*clientJournalInventory, error) {
	result := &clientJournalInventory{storage: emptyClientStorage()}
	f, err := journal.FS().Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	directory, ok := f.(fs.ReadDirFile)
	if !ok {
		return nil, syncproto.ErrInvalid
	}
	count := 0
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			count++
			if count > clientJournalLimit+clientReceiptLimit {
				return nil, syncproto.ErrLimit
			}
			if !entry.IsDir() || !syncproto.ValidOperationID(entry.Name()) {
				return nil, syncproto.ErrInvalid
			}
			item, err := inspectClientInventoryEntry(journal, entry.Name())
			if err != nil {
				return nil, err
			}
			result.adjust(clientInventoryEntry{}, item)
			if result.storage.ActiveOperations > clientJournalLimit || result.storage.RetainedReceipts > clientReceiptLimit || result.storage.RecoveryBytes > clientRecoveryBytes {
				return nil, syncproto.ErrLimit
			}
		}
		if errors.Is(readErr, io.EOF) {
			return result, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func validateClientMutationRecord(record clientMutationRecord, id string) error {
	if record.InspectionConfirmation != "" && (!syncproto.ValidHash(record.InspectionConfirmation) || record.Retirement == "") {
		return syncproto.ErrInvalid
	}
	request := record.Request
	request.Generation = "journal-validation"
	digest, err := request.Digest()
	if err != nil || digest != record.Digest || request.ID != id || record.Result.ID != id || (record.Result.Status != "applied" && record.Result.Status != "uncertain") || (record.Retirement != "" && record.Retirement != "retiring" && record.Retirement != "retired") || (record.Retirement != "" && (record.Result.Status != "applied" || record.Result.Recovery)) {
		return syncproto.ErrInvalid
	}
	hasBefore := request.Before != nil && request.Before.Kind == "file"
	if record.Result.Recovery && !hasBefore || record.Retirement == "" && record.Result.Status == "applied" && record.Result.Recovery != hasBefore {
		return syncproto.ErrInvalid
	}
	return nil
}

func loadClientMutationRecord(operation *safefs.Root, id string) (clientMutationRecord, error) {
	var record clientMutationRecord
	raw, err := operation.ReadAll("record.json", 16<<10)
	if err != nil {
		return record, err
	}
	if json.Unmarshal(raw, &record) != nil || validateClientMutationRecord(record, id) != nil {
		return record, syncproto.ErrInvalid
	}
	return record, nil
}

func clientOperationStatus(record clientMutationRecord, server, workspace string) syncproto.OperationStatus {
	// The digest already covers all original path/content/project/device fields.
	// Instance and workspace prevent confirmation reuse across restored servers.
	raw, _ := json.Marshal([]string{"agentbox-recovery-retirement-v1", server, workspace, record.Result.ID, record.Digest, record.Request.Device, record.Request.Project})
	return syncproto.OperationStatus{Operation: record.Result, Path: record.Request.Path, Kind: record.Request.Kind, Before: record.Request.Before, After: record.Request.After, Digest: record.Digest, Device: record.Request.Device, Project: record.Request.Project, Retirement: record.Retirement, RetirementConfirmation: syncproto.HashBytes(raw)}
}

func (s *Server) handleClientStorage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	storage := emptyClientStorage()
	err := s.workspaces().WithSession(ctx, sess.ID, func(current store.Session) error {
		session, err := s.openDataDir(s.sessionDir(current))
		if err != nil {
			return err
		}
		defer session.Close()
		journal, err := session.Sub("client-sync")
		if os.IsNotExist(err) {
			s.forgetClientInventory(current.ID)
			return nil
		}
		if err != nil {
			return err
		}
		defer journal.Close()
		inventory, err := s.clientInventory(ctx, current.ID, journal)
		if err == nil {
			storage = inventory.storage
		}
		return err
	})
	if err != nil {
		writeMutationError(w, err, syncproto.MutationResult{})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, storage)
}

func (s *Server) handleClientRetire(w http.ResponseWriter, r *http.Request, sess store.Session) {
	serverID, err := s.clientIdentity()
	if err != nil || r.Header.Get("X-Agentbox-Server-ID") != serverID || serverID == "" {
		writeErr(w, http.StatusConflict, "清理必须绑定已确认的服务器身份")
		return
	}
	id := r.PathValue("operation")
	var request syncproto.RetireOperationRequest
	if !syncproto.ValidOperationID(id) || decodeClientRequest(w, r, &request) != nil || request.Validate() != nil {
		writeErr(w, http.StatusBadRequest, "清理确认参数无效")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var record clientMutationRecord
	err = s.workspaces().WithSession(ctx, sess.ID, func(current store.Session) (err error) {
		defer func() {
			if err != nil {
				s.forgetClientInventory(current.ID)
			}
		}()
		if s.syncLeases().WorkspaceActive(current.ID) {
			return syncproto.ErrChanged
		}
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
		record, err = loadClientMutationRecord(operation, id)
		if err != nil {
			return err
		}
		status := clientOperationStatus(record, serverID, current.ID)
		if record.Result.Status != "applied" || request.Device != status.Device || request.Digest != status.Digest || request.Confirmation != status.RetirementConfirmation {
			return syncproto.ErrChanged
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
		if err = retireClientOperation(ctx, operation, &record, nil); err != nil {
			return err
		}
		after, err := inspectClientInventoryEntry(journal, id)
		if err != nil {
			return err
		}
		inventory.adjust(before, after)
		return nil
	})
	if err != nil {
		writeMutationError(w, err, syncproto.MutationResult{})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, clientOperationStatus(record, serverID, sess.ID))
}

// checkpoint injects crash boundaries in tests. There is deliberately no
// directory removal: old binaries still see an applied receipt at this ID and
// can never mistake a retired operation for permission to execute it again.
func retireClientOperation(ctx context.Context, operation *safefs.Root, record *clientMutationRecord, checkpoint func(string) error) error {
	guard := func(stage string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if checkpoint != nil {
			return checkpoint(stage)
		}
		return nil
	}
	if record.Result.Status != "applied" {
		return syncproto.ErrChanged
	}
	if record.Retirement == "retired" {
		return nil
	}
	// Validate old bytes before recording intent, and again before unlink. A
	// missing file is only acceptable on a persisted retiring retry.
	verify := func() error {
		info, err := operation.Lstat("before")
		if os.IsNotExist(err) {
			if record.Retirement == "retiring" || !record.Result.Recovery {
				return nil
			}
			return syncproto.ErrChanged
		}
		if err != nil {
			return err
		}
		if record.Request.Before == nil || record.Request.Before.Kind != "file" {
			return syncproto.ErrInvalid
		}
		actual, err := copyManifestFile(ctx, operation, "before", info, io.Discard)
		if err != nil {
			return err
		}
		if actual.Hash != record.Request.Before.Hash || actual.Size != record.Request.Before.Size {
			return syncproto.ErrChanged
		}
		return nil
	}
	if err := guard("before_intent"); err != nil {
		return err
	}
	if err := verify(); err != nil {
		return err
	}
	if record.Retirement == "" {
		record.Retirement = "retiring"
		record.Result.Recovery = false
		if err := saveClientMutation(operation, *record); err != nil {
			return err
		}
	}
	if err := guard("after_intent"); err != nil {
		return err
	}
	if err := verify(); err != nil {
		return err
	}
	if err := guard("before_unlink"); err != nil {
		return err
	}
	if err := operation.Remove("before"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := guard("after_unlink"); err != nil {
		return err
	}
	if err := operation.Sync(); err != nil {
		return err
	}
	if err := guard("after_unlink_sync"); err != nil {
		return err
	}
	record.Retirement = "retired"
	if err := saveClientMutation(operation, *record); err != nil {
		return err
	}
	return guard("after_retired")
}
