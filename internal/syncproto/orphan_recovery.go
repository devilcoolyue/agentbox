package syncproto

import "encoding/json"

const RecoveryPageLimit = 50

// RecoveryListItem deliberately keeps unparseable journals visible without
// inventing a device, path, completion state, or permission to discard them.
type RecoveryListItem struct {
	ID     string           `json:"id"`
	Status *OperationStatus `json:"status,omitempty"`
	Issue  string           `json:"issue,omitempty"`
}

type RecoveryOperationsPage struct {
	ServerID   string             `json:"server_id"`
	Workspace  string             `json:"workspace"`
	Device     string             `json:"device"`
	Items      []RecoveryListItem `json:"items"`
	NextCursor string             `json:"next_cursor"`
}

type RecoveryOperationReview struct {
	ServerID        string          `json:"server_id"`
	Workspace       string          `json:"workspace"`
	Status          OperationStatus `json:"status"`
	Comparison      string          `json:"comparison"`
	Current         *Entry          `json:"current,omitempty"`
	ProjectPath     string          `json:"project_path,omitempty"`
	ProjectRevision int64           `json:"project_revision,omitempty"`
	RecoveryState   string          `json:"recovery_state"`
	LeaseActive     bool            `json:"lease_active"`
	LocalPending    bool            `json:"local_pending"`
	CanRetire       bool            `json:"can_retire"`
	Digest          string          `json:"digest"`
}

func (p RecoveryOperationsPage) Validate(server, workspace, device string) error {
	if p.ServerID != server || p.Workspace != workspace || p.Device != device || p.Items == nil || len(p.Items) > RecoveryPageLimit || len(p.NextCursor) > 4096 {
		return ErrInvalid
	}
	previous := ""
	for _, item := range p.Items {
		if !ValidOperationID(item.ID) || item.ID <= previous {
			return ErrInvalid
		}
		previous = item.ID
		if item.Status == nil {
			if device != "" || item.Issue != "missing_record" && item.Issue != "invalid_record" {
				return ErrInvalid
			}
		} else if item.Issue != "" || item.Status.Validate(item.ID) != nil || !ValidHash(item.Status.Digest) || device != "" && item.Status.Device != device {
			return ErrInvalid
		}
	}
	return nil
}

// Mutable disposal/availability flags are excluded so a saved authorization can
// finish after a lost response. The server separately persists this exact digest
// with its retirement intent; an unstarted request must match a fresh comparison.
func (r RecoveryOperationReview) Confirmation() string {
	raw, _ := json.Marshal(struct {
		Domain, Server, Workspace, Operation, Device, Intent, Comparison, ProjectPath string
		ProjectRevision                                                               int64
		Current                                                                       *Entry
	}{"agentbox-orphan-recovery-v1", r.ServerID, r.Workspace, r.Status.Operation.ID, r.Status.Device, r.Status.Digest, r.Comparison, r.ProjectPath, r.ProjectRevision, r.Current})
	return HashBytes(raw)
}

func (r RecoveryOperationReview) Validate(server, workspace, device, id string) error {
	if r.ServerID != server || r.Workspace != workspace || r.Status.Device != device || r.Status.Validate(id) != nil || !ValidHash(r.Status.Digest) || r.Digest != r.Confirmation() {
		return ErrInvalid
	}
	switch r.Comparison {
	case "matches_after", "matches_before", "changed", "project_missing", "project_changed", "unavailable":
	default:
		return ErrInvalid
	}
	switch r.RecoveryState {
	case "available", "missing", "corrupt", "none", "retiring", "retired":
	default:
		return ErrInvalid
	}
	if r.Current != nil && !validFileEntry(r.Current) && !validDirectoryEntry(r.Current) {
		return ErrInvalid
	}
	if r.CanRetire && (r.Status.Operation.Status != "applied" || r.LeaseActive || r.LocalPending || r.Status.Retirement == "retired" || r.RecoveryState == "missing" || r.RecoveryState == "corrupt") {
		return ErrInvalid
	}
	return nil
}

type RecoveryInspectRetireRequest struct {
	Device       string `json:"device"`
	Confirmation string `json:"confirmation"`
}

func (r RecoveryInspectRetireRequest) Validate() error {
	if !validIdentity(r.Device) || !ValidHash(r.Confirmation) {
		return ErrInvalid
	}
	return nil
}
