package syncclient

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

const historyPageBatches = 20
const historyPageFiles = 50

// Cursors select metadata only, never filesystem paths or mutation authority.
// Every page is read in one SQLite transaction and pinned to the binding's
// revision. A new batch, reconciliation or archive invalidates older cursors.
type historyCursor struct {
	Binding  string `json:"b"`
	Revision int64  `json:"r"`
	Batch    int    `json:"n"`
	File     int    `json:"f"`
}

type HistoryPage struct {
	RemoteStorage       *syncproto.RecoveryStorage `json:"remote_storage"`
	RemoteStorageStatus string                     `json:"remote_storage_status"`
	History             []RecoveryBatch            `json:"history"`
	NextCursor          string                     `json:"next_cursor"`
	Revision            int64                      `json:"revision"`
	TotalBatches        int                        `json:"total_batches"`
	Pending             bool                       `json:"pending"`
	Metadata            MetadataUsage              `json:"metadata"`
	LocalRecovery       *syncfs.RecoveryUsage      `json:"local_recovery"`
	LocalRecoveryStatus string                     `json:"local_recovery_status"`
}

type MetadataUsage struct {
	Bindings       int   `json:"bindings"`
	BindingLimit   int   `json:"binding_limit"`
	Bytes          int64 `json:"bytes"`
	ByteLimit      int64 `json:"byte_limit"`
	HistoryBatches int   `json:"history_batches"`
	HistoryLimit   int   `json:"history_limit"`
	BindingBytes   int64 `json:"binding_bytes"`
}

func (e *Engine) RecoveryHistoryPage(ctx context.Context, id, cursor string) (HistoryPage, error) {
	saved, remote, err := e.recoveryBinding(ctx, id)
	if err != nil {
		return HistoryPage{}, err
	}
	progressStage(ctx, "history")
	page, err := e.State.historyPage(ctx, saved, cursor)
	if err != nil {
		return HistoryPage{}, err
	}
	page.RemoteStorageStatus = "server_changed"
	if remote != nil {
		page.RemoteStorageStatus = "unavailable"
		identity, identityErr := remote.Identity(ctx)
		if identityErr == nil && identity.ServerID == saved.Binding.ServerID && identity.User == saved.Binding.User {
			if identity.Features["sync_recovery_gc"] != 1 {
				page.RemoteStorageStatus = "unsupported"
			} else {
				usage, usageErr := remote.RecoveryStorage(ctx, saved.Binding.Workspace)
				if usageErr == nil {
					page.RemoteStorage = &usage
					page.RemoteStorageStatus = "available"
				}
			}
		}
	}
	page.LocalRecoveryStatus = "unavailable"
	root, openErr := syncfs.OpenContext(ctx, saved.Directory)
	if openErr == nil {
		defer root.Close()
		identity, identityErr := root.Identity()
		if identityErr == nil && identity == saved.Binding.LocalID {
			usage, usageErr := root.RecoveryUsage(ctx)
			if usageErr == nil {
				page.LocalRecovery = &usage
				page.LocalRecoveryStatus = "available"
			}
		}
	}
	if err = ctx.Err(); err != nil {
		return HistoryPage{}, err
	}
	return page, nil
}

func (s *StateStore) historyPage(ctx context.Context, saved SavedBinding, cursor string) (HistoryPage, error) {
	page := HistoryPage{History: []RecoveryBatch{}, Revision: saved.Revision, Pending: saved.Pending != nil}
	position := historyCursor{Binding: saved.ID, Revision: saved.Revision}
	if cursor != "" {
		if len(cursor) > 512 {
			return HistoryPage{}, syncproto.ErrInvalid
		}
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &position) != nil || position.Batch < 0 || position.File < 0 {
			return HistoryPage{}, syncproto.ErrInvalid
		}
		if position.Binding != saved.ID || position.Revision != saved.Revision {
			return HistoryPage{}, ErrStateChanged
		}
	}
	if err := s.root.CheckIdentity(); err != nil {
		return HistoryPage{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return HistoryPage{}, err
	}
	defer tx.Rollback()
	var revision int64
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM bindings WHERE id=?", saved.ID).Scan(&revision); err != nil {
		return HistoryPage{}, err
	}
	if revision != saved.Revision {
		return HistoryPage{}, ErrStateChanged
	}
	usage := MetadataUsage{BindingLimit: 1024, ByteLimit: maxStoredMetadata, HistoryLimit: 1000}
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM bindings),
 (SELECT coalesce(sum(length(state)),0) FROM bindings)+(SELECT coalesce(sum(length(state)),0) FROM batches),
 (SELECT count(*) FROM batches WHERE binding=?),
 (SELECT length(state) FROM bindings WHERE id=?)+(SELECT coalesce(sum(length(state)),0) FROM batches WHERE binding=?)`, saved.ID, saved.ID, saved.ID).Scan(&usage.Bindings, &usage.Bytes, &usage.HistoryBatches, &usage.BindingBytes); err != nil {
		return HistoryPage{}, err
	}
	if usage.HistoryBatches > usage.HistoryLimit {
		return HistoryPage{}, syncproto.ErrLimit
	}
	page.Metadata = usage
	page.TotalBatches = usage.HistoryBatches
	if page.Pending {
		page.TotalBatches++
	}
	if position.Batch > page.TotalBatches || position.Batch == page.TotalBatches && position.File != 0 {
		return HistoryPage{}, syncproto.ErrInvalid
	}
	filesLeft := historyPageFiles
	for position.Batch < page.TotalBatches && len(page.History) < historyPageBatches && filesLeft > 0 {
		if err = ctx.Err(); err != nil {
			return HistoryPage{}, err
		}
		pending := page.Pending && position.Batch == 0
		var batch Batch
		if pending {
			batch = *saved.Pending
		} else {
			offset := position.Batch
			if page.Pending {
				offset--
			}
			var raw []byte
			var batchID string
			if err = tx.QueryRowContext(ctx, "SELECT id,state FROM batches WHERE binding=? ORDER BY rowid DESC LIMIT 1 OFFSET ?", saved.ID, offset).Scan(&batchID, &raw); err != nil {
				return HistoryPage{}, err
			}
			if len(raw) > maxStateJSON || json.Unmarshal(raw, &batch) != nil || batch.ID != batchID {
				return HistoryPage{}, syncproto.ErrInvalid
			}
		}
		if err = validateRecoveryBatch(batch, saved.Binding); err != nil {
			return HistoryPage{}, err
		}
		summary := recoveryWindow(batch, pending, position.File, filesLeft)
		if position.File > summary.TotalFiles || position.File == summary.TotalFiles && position.File != 0 {
			return HistoryPage{}, syncproto.ErrInvalid
		}
		page.History = append(page.History, summary)
		filesLeft -= len(summary.Files)
		position.File += len(summary.Files)
		if position.File == summary.TotalFiles {
			position.Batch++
			position.File = 0
		}
	}
	if position.Batch < page.TotalBatches {
		raw, _ := json.Marshal(position)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	if err = s.root.CheckIdentity(); err != nil {
		return HistoryPage{}, err
	}
	if err = tx.Commit(); err != nil {
		return HistoryPage{}, err
	}
	return page, nil
}
