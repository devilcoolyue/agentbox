// Package syncclient implements portable file planning and the sidecar protocol.
package syncclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

const DesktopProtocol = 1

// A preview contains bounded manifest metadata and operations. Keep this well
// below an unbounded IPC message while allowing projects with many files.
const maxCommand = 4 << 20

type command struct {
	Workspace       string               `json:"workspace,omitempty"`
	ServerID        string               `json:"server_id,omitempty"`
	Device          string               `json:"device,omitempty"`
	Choices         map[string]Direction `json:"choices,omitempty"`
	BasisDigest     string               `json:"basis_digest,omitempty"`
	Cursor          string               `json:"cursor,omitempty"`
	Revision        int64                `json:"revision,omitempty"`
	Progress        bool                 `json:"progress,omitempty"`
	Version         int                  `json:"version"`
	ID              string               `json:"id"`
	Type            string               `json:"type"`
	Directory       string               `json:"directory,omitempty"`
	Server          string               `json:"server,omitempty"`
	User            string               `json:"user,omitempty"`
	Token           string               `json:"token,omitempty"`
	AllowHTTP       bool                 `json:"allow_http,omitempty"`
	StateDir        string               `json:"state_dir,omitempty"`
	Binding         *SyncBinding         `json:"binding,omitempty"`
	BindingID       string               `json:"binding_id,omitempty"`
	Direction       Direction            `json:"direction,omitempty"`
	Preview         *Preview             `json:"preview,omitempty"`
	Confirmation    string               `json:"confirmation,omitempty"`
	Action          string               `json:"action,omitempty"`
	BatchID         string               `json:"batch_id,omitempty"`
	OperationID     string               `json:"operation_id,omitempty"`
	ExportDirectory string               `json:"export_directory,omitempty"`
}

type event struct {
	OrphanPage   *syncproto.RecoveryOperationsPage  `json:"orphan_page,omitempty"`
	OrphanReview *syncproto.RecoveryOperationReview `json:"orphan_review,omitempty"`
	Cleanup      *RemoteCleanupReview               `json:"cleanup,omitempty"`
	Abandon      *AbandonReview                     `json:"abandon,omitempty"`
	Page         *HistoryPage                       `json:"page,omitempty"`
	Progress     *Progress                          `json:"progress,omitempty"`
	Version      int                                `json:"version"`
	ID           string                             `json:"id,omitempty"`
	Type         string                             `json:"type"`
	Capabilities []string                           `json:"capabilities,omitempty"`
	Error        string                             `json:"error,omitempty"`
	Inspection   *Inspection                        `json:"inspection,omitempty"`
	Binding      *BindingView                       `json:"binding,omitempty"`
	Preview      *Preview                           `json:"preview,omitempty"`
	Bindings     []BindingView                      `json:"bindings,omitempty"`
	Review       *PendingReview                     `json:"review,omitempty"`
	History      []RecoveryBatch                    `json:"history,omitempty"`
	Filename     string                             `json:"filename,omitempty"`
}

// SyncBinding is the renderer-free binding input. ServerID and LocalID are
// filled by the sidecar after authenticated identity and native directory
// probing; callers cannot assert either identity themselves.
type SyncBinding struct {
	Workspace   string `json:"workspace"`
	Project     string `json:"project"`
	ProjectPath string `json:"project_path"`
}

// RunDesktop serves newline-delimited JSON on private inherited pipes. EOF is
// the parent-death signal. Inspection receives a native-picker-selected directory
// through the private pipe. Sync commands receive credentials only on this pipe;
// EOF cancels all work. Nothing starts without an explicit native request.
// stdout is protocol-only. Diagnostics must go to the caller's stderr.
func RunDesktop(input io.Reader, output io.Writer) error {

	var outputMu sync.Mutex
	emit := func(value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if len(raw)+1 > maxCommand {
			response, ok := value.(event)
			if !ok {
				return fmt.Errorf("desktop response exceeds limit")
			}
			raw, _ = json.Marshal(event{Version: DesktopProtocol, ID: response.ID, Type: "error", Error: "sync_limit"})
		}
		outputMu.Lock()
		defer outputMu.Unlock()
		_, err = output.Write(append(raw, '\n'))
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	var workerMu sync.Mutex
	busy := false
	syncBusy := false
	if err := emit(struct {
		Version      int      `json:"version"`
		Type         string   `json:"type"`
		Capabilities []string `json:"capabilities"`
	}{DesktopProtocol, "ready", []string{"local_inspect_v1", "sync_batch_v1"}}); err != nil {
		return fmt.Errorf("write handshake: %w", err)
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), maxCommand)
	for scanner.Scan() {
		var request command
		response := event{Version: DesktopProtocol, Type: "error"}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			response.Error = "invalid_command"
		} else if request.Version != DesktopProtocol {
			response.Error = "unsupported_version"
		} else if request.ID == "" || len(request.ID) > 128 {
			response.Error = "invalid_id"
		} else {
			response.ID = request.ID
			switch request.Type {
			case "ping":
				response.Type = "pong"
			case "shutdown":
				cancel()
				workers.Wait()
				response.Type = "stopped"
			case "inspect":
				if request.Directory == "" || len(request.Directory) > 4096 {
					response.Error = "invalid_directory"
					break
				}
				workerMu.Lock()
				if busy || syncBusy {
					workerMu.Unlock()
					response.Error = "inspection_busy"
					break
				}
				busy = true
				workerMu.Unlock()
				workers.Add(1)
				go func(request command) {
					defer workers.Done()
					inspection, err := InspectLocal(ctx, request.Directory)
					result := event{Version: DesktopProtocol, ID: request.ID, Type: "inspected", Inspection: &inspection}
					if err != nil {
						result.Type = "error"
						result.Error = inspectionError(err)
						result.Inspection = nil
					}
					workerMu.Lock()
					busy = false
					workerMu.Unlock()
					if ctx.Err() == nil {
						_ = emit(result)
					}
				}(request)
				continue
			case "sync_orphan_list", "sync_orphan_review", "sync_orphan_export", "sync_orphan_retire", "sync_remote_cleanup_review", "sync_remote_cleanup", "sync_recovery_discard", "sync_history_cleanup", "sync_abandon_review", "sync_abandon", "sync_archive", "sync_bind", "sync_preview", "sync_apply", "sync_list", "sync_review", "sync_resolve", "sync_history", "sync_export":
				workerMu.Lock()
				if busy || syncBusy {
					workerMu.Unlock()
					response.Error = "sync_busy"
					break
				}
				syncBusy = true
				workerMu.Unlock()
				workers.Add(1)
				go func(request command) {
					defer workers.Done()
					work := ctx
					var pump *progressPump
					if request.Progress {
						pump = startProgressPump(ctx, func(value Progress) {
							if err := emit(event{Version: DesktopProtocol, ID: request.ID, Type: "sync_progress", Progress: &value}); err != nil {
								cancel()
							}
						})
						work = WithProgress(ctx, pump.offer)
					}
					result := syncEvent(work, request)
					if pump != nil {
						pump.finish()
					}
					workerMu.Lock()
					syncBusy = false
					workerMu.Unlock()
					if ctx.Err() == nil {
						_ = emit(result)
					}
				}(request)
				continue
			default:
				response.Error = "unsupported_command"
			}
		}
		if err := emit(response); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
		if response.Type == "stopped" {
			return nil
		}
	}
	if scanner.Err() != nil {
		return fmt.Errorf("desktop input failed or exceeded limit")
	}
	return nil
}

func syncEvent(parent context.Context, request command) event {
	ctx, cancel := context.WithTimeout(parent, 15*time.Minute)
	defer cancel()
	result := event{Version: DesktopProtocol, ID: request.ID, Type: "error"}
	progressStage(ctx, "authenticating")
	if request.Server == "" || request.Token == "" || request.User == "" || request.StateDir == "" {
		result.Error = "sync_credentials_missing"
		return result
	}
	remote, err := NewRemote(request.Server, request.Token, request.AllowHTTP)
	if err != nil {
		result.Error = "sync_invalid_request"
		return result
	}
	defer remote.Close()
	identity, err := remote.Identity(ctx)
	if err != nil || identity.User != request.User {
		result.Error = "sync_identity"
		return result
	}
	orphan := request.Type == "sync_orphan_list" || request.Type == "sync_orphan_review" || request.Type == "sync_orphan_export" || request.Type == "sync_orphan_retire"
	if !orphan && identity.Features["sync"] != syncproto.Version {
		result.Error = "sync_disabled"
		return result
	}
	remote.serverID = identity.ServerID
	state, err := OpenStateContext(ctx, request.StateDir)
	if err != nil {
		result.Error = "sync_state_unavailable"
		return result
	}
	defer state.Close()

	switch request.Type {
	case "sync_orphan_list", "sync_orphan_review", "sync_orphan_export", "sync_orphan_retire":
		engine := &Engine{State: state, Remote: remote}
		scope := OrphanRecoveryScope{ServerID: request.ServerID, User: request.User, Workspace: request.Workspace, Device: request.Device, OperationID: request.OperationID}
		switch request.Type {
		case "sync_orphan_list":
			page, e := engine.ListOrphanRecovery(ctx, scope, request.Cursor)
			err = e
			result.Type = "sync_orphan_listed"
			result.OrphanPage = &page
		case "sync_orphan_review":
			review, e := engine.ReviewOrphanRecovery(ctx, scope)
			err = e
			result.Type = "sync_orphan_reviewed"
			result.OrphanReview = &review
		case "sync_orphan_export":
			if request.ExportDirectory == "" {
				err = syncproto.ErrInvalid
				break
			}
			result.Filename, err = engine.ExportOrphanRecovery(ctx, scope, request.ExportDirectory)
			result.Type = "sync_orphan_exported"
		case "sync_orphan_retire":
			review, e := engine.RetireOrphanRecovery(ctx, scope, request.Confirmation)
			err = e
			result.Type = "sync_orphan_retired"
			result.OrphanReview = &review
		}
		if err != nil {
			return event{Version: DesktopProtocol, ID: request.ID, Type: "error", Error: syncErrorCode(err)}
		}
		return result
	case "sync_bind":
		if request.Binding == nil || request.Directory == "" {
			result.Error = "sync_invalid_request"
			return result
		}
		root, err := syncfs.OpenContext(ctx, request.Directory)
		if err != nil {
			result.Error = "sync_local_directory"
			if errors.Is(err, syncfs.ErrUnsupportedVolume) {
				result.Error = "sync_unsupported_volume"
			}
			return result
		}
		caps, err := root.Probe()
		root.Close()
		if err != nil {
			result.Error = "sync_local_directory"
			if errors.Is(err, syncfs.ErrUnsupportedVolume) {
				result.Error = "sync_unsupported_volume"
			}
			return result
		}
		server, err := syncproto.NormalizeServer(request.Server)
		if err != nil {
			result.Error = "sync_invalid_request"
			return result
		}
		binding := syncproto.Binding{Version: syncproto.Version, Server: server, ServerID: identity.ServerID, User: identity.User, Workspace: request.Binding.Workspace, Project: request.Binding.Project, ProjectPath: request.Binding.ProjectPath, LocalID: caps.DirectoryID}
		progressStage(ctx, "remote_scan")
		manifest, err := remote.Manifest(ctx, binding.Workspace, binding.Project)
		if err != nil {
			result.Error = syncErrorCode(err)
			return result
		}
		if manifest.ProjectPath != binding.ProjectPath {
			result.Error = "sync_binding"
			return result
		}
		if err = ctx.Err(); err != nil {
			result.Error = syncErrorCode(err)
			return result
		}
		saved, err := state.RegisterContext(ctx, binding, request.Directory)
		if err != nil {
			result.Error = syncErrorCode(err)
			return result
		}
		result.Type = "sync_bound"
		result.Binding = bindingView(saved)
		return result
	case "sync_list":
		saved, err := state.BindingsForServer(remote.base.String(), identity.User)
		if err != nil {
			result.Error = syncErrorCode(err)
			return result
		}
		result.Bindings = []BindingView{}
		for _, value := range saved {
			view := bindingView(value)
			view.ServerChanged = value.Binding.ServerID != identity.ServerID
			result.Bindings = append(result.Bindings, *view)
		}
		result.Type = "sync_listed"
		return result
	case "sync_archive":
		if !syncproto.ValidOperationID(request.BindingID) || request.Revision < 1 {
			result.Error = "sync_invalid_request"
			return result
		}
		engine := &Engine{State: state, Remote: remote}
		saved, err := engine.ArchiveBinding(ctx, request.BindingID, request.Revision)
		if err != nil {
			result.Error = syncErrorCode(err)
			return result
		}
		result.Type = "sync_archived"
		result.Binding = bindingView(saved)
		return result
	case "sync_preview":
		if !syncproto.ValidOperationID(request.BindingID) {
			result.Error = "sync_invalid_request"
			return result
		}
		engine := &Engine{State: state, Remote: remote}
		preview, err := engine.PreviewChoices(ctx, request.BindingID, request.Direction, request.Choices, request.BasisDigest)
		if err != nil {
			result.Error = syncErrorCode(err)
			return result
		}
		result.Type = "sync_previewed"
		result.Preview = &preview
		return result
	case "sync_remote_cleanup_review", "sync_remote_cleanup", "sync_recovery_discard", "sync_history_cleanup", "sync_abandon_review", "sync_abandon", "sync_review", "sync_resolve", "sync_history", "sync_export":
		if !syncproto.ValidOperationID(request.BindingID) {
			result.Error = "sync_invalid_request"
			return result
		}
		engine := &Engine{State: state, Remote: remote}
		switch request.Type {
		case "sync_remote_cleanup_review":
			review, err := engine.ReviewRemoteCleanup(ctx, request.BindingID, request.BatchID, request.Revision)
			if err != nil {
				result.Error = syncErrorCode(err)
				return result
			}
			result.Type = "sync_remote_cleanup_reviewed"
			result.Cleanup = &review
		case "sync_remote_cleanup":
			saved, err := engine.CleanupRemoteRecovery(ctx, request.BindingID, request.BatchID, request.Revision, request.Confirmation)
			if err != nil {
				result.Error = syncErrorCode(err)
				return result
			}
			result.Type = "sync_remote_cleaned"
			result.Binding = bindingView(saved)
		case "sync_recovery_discard":
			saved, err := engine.DiscardLocalRecovery(ctx, request.BindingID, request.BatchID, request.OperationID, request.Revision)
			if err != nil {
				result.Error = syncErrorCode(err)
				return result
			}
			result.Type = "sync_recovery_discarded"
			result.Binding = bindingView(saved)
		case "sync_history_cleanup":
			saved, err := engine.CleanupHistory(ctx, request.BindingID, request.BatchID, request.Revision)
			if err != nil {
				result.Error = syncErrorCode(err)
				return result
			}
			result.Type = "sync_history_cleaned"
			result.Binding = bindingView(saved)
		case "sync_abandon_review":
			report, err := engine.ReviewAbandon(ctx, request.BindingID)
			if err != nil {
				result.Error = syncErrorCode(err)
				return result
			}
			result.Type = "sync_abandon_reviewed"
			result.Abandon = &report
		case "sync_abandon":
			saved, err := engine.AbandonPending(ctx, request.BindingID, request.Confirmation)
			if err != nil {
				result.Error = syncErrorCode(err)
				return result
			}
			result.Type = "sync_abandoned"
			result.Binding = bindingView(saved)
		case "sync_review":
			review, err := engine.ReviewPending(ctx, request.BindingID)
			if err != nil {
				result.Error = syncErrorCode(err)
				return result
			}
			result.Type = "sync_reviewed"
			result.Review = &review
		case "sync_resolve":
			saved, err := engine.ResolvePending(ctx, request.BindingID, request.Confirmation, request.Action)
			if err != nil {
				result.Error = syncErrorCode(err)
				return result
			}
			result.Type = "sync_resolved"
			result.Binding = bindingView(saved)
		case "sync_history":
			page, err := engine.RecoveryHistoryPage(ctx, request.BindingID, request.Cursor)
			if err != nil {
				result.Error = syncErrorCode(err)
				return result
			}
			result.Type = "sync_history"
			result.History = page.History
			result.Page = &page
		case "sync_export":
			if request.ExportDirectory == "" {
				result.Error = "sync_invalid_request"
				return result
			}
			name, err := engine.ExportRecovery(ctx, request.BindingID, request.BatchID, request.OperationID, request.ExportDirectory)
			if err != nil {
				result.Error = syncErrorCode(err)
				return result
			}
			result.Type = "sync_exported"
			result.Filename = name
		}
		return result
	case "sync_apply":
		if !syncproto.ValidOperationID(request.BindingID) || request.Preview == nil {
			result.Error = "sync_invalid_request"
			return result
		}
		engine := &Engine{State: state, Remote: remote}
		saved, err := engine.Apply(ctx, request.BindingID, *request.Preview, request.Confirmation)
		if err != nil {
			result.Error = syncErrorCode(err)
			result.Binding = bindingView(saved)
			return result
		}
		result.Type = "sync_applied"
		result.Binding = bindingView(saved)
		return result
	default:
		result.Error = "sync_invalid_request"
		return result
	}
}

func syncErrorCode(err error) string {
	switch {
	case errors.Is(err, syncfs.ErrUnsupportedVolume):
		return "sync_unsupported_volume"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "sync_canceled"
	case errors.Is(err, ErrSyncUnavailable):
		return "sync_disabled"
	case errors.Is(err, ErrPending):
		return "sync_pending"
	case errors.Is(err, ErrBinding):
		return "sync_binding"
	case errors.Is(err, ErrRulesChanged):
		return "sync_rules_changed"
	case errors.Is(err, ErrStateChanged):
		return "sync_stale"
	case errors.Is(err, syncproto.ErrLeaseExpired):
		return "sync_lease_expired"
	case errors.Is(err, syncproto.ErrChanged):
		return "sync_changed"
	case errors.Is(err, syncproto.ErrLimit):
		return "sync_limit"
	default:
		return "sync_failed"
	}
}

// BindingView keeps large manifests and operation journals inside the sidecar.
// It exposes only the metadata needed for selection and pending-state display.
type BindingView struct {
	Archived      bool              `json:"archived"`
	ServerChanged bool              `json:"server_changed"`
	ID            string            `json:"id"`
	Revision      int64             `json:"revision"`
	Binding       syncproto.Binding `json:"binding"`
	Directory     string            `json:"directory"`
	Pending       bool              `json:"pending"`
}

func bindingView(saved SavedBinding) *BindingView {
	if saved.ID == "" {
		return nil
	}
	return &BindingView{Archived: saved.Archived, ID: saved.ID, Revision: saved.Revision, Binding: saved.Binding, Directory: saved.Directory, Pending: saved.Pending != nil}
}
