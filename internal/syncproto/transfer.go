package syncproto

import "encoding/json"

// FileRequest binds a download to the project and rules seen in its preview.
// Expected includes the executable bit, since a chmod is also a remote change.
type FileRequest struct {
	Project   string `json:"project"`
	Revision  int64  `json:"project_revision"`
	RulesHash string `json:"rules_hash"`
	Path      string `json:"path"`
	Expected  Entry  `json:"expected"`
}

func (r FileRequest) Validate() error {
	if !validIdentity(r.Project) || r.Revision < 1 || !ValidHash(r.RulesHash) || !ValidPath(r.Path) || r.Expected.Kind != "file" || !ValidHash(r.Expected.Hash) || r.Expected.Size < 0 || r.Expected.Size > DefaultMaxFileBytes {
		return ErrInvalid
	}
	return nil
}

type ManifestResponse struct {
	Manifest    Manifest `json:"manifest"`
	Digest      string   `json:"digest"`
	Project     string   `json:"project"`
	Revision    int64    `json:"project_revision"`
	ProjectPath string   `json:"project_path,omitempty"`
}

func (m ManifestResponse) Validate(project string) error {
	if m.Project != project || m.Revision < 1 || m.ProjectPath != "" && m.ProjectPath != "." && !ValidPath(m.ProjectPath) {
		return ErrInvalid
	}
	digest, err := m.Manifest.Digest()
	if err != nil {
		return err
	}
	if digest != m.Digest {
		return ErrInvalid
	}
	return nil
}

// Mutation identifies one durable remote operation. Generation fences this
// attempt, but is excluded from the intent digest so a reacquired lease can
// look up (never blindly reapply) a previously submitted operation.
type Mutation struct {
	Version    int    `json:"version"`
	ID         string `json:"operation_id"`
	Project    string `json:"project"`
	Revision   int64  `json:"project_revision"`
	RulesHash  string `json:"rules_hash"`
	Device     string `json:"device"`
	Generation string `json:"generation"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Before     *Entry `json:"before"`
	After      *Entry `json:"after"`
}

func ValidOperationID(id string) bool { return len(id) == 32 && ValidHash(id+id) }
func validFileEntry(e *Entry) bool {
	return e != nil && e.Kind == "file" && ValidHash(e.Hash) && e.Size >= 0 && e.Size <= DefaultMaxFileBytes
}
func validDirectoryEntry(e *Entry) bool { return e != nil && *e == (Entry{Kind: "directory"}) }
func (m Mutation) Validate() error {
	if m.Version != Version || !ValidOperationID(m.ID) || !validIdentity(m.Project) || m.Revision < 1 || !ValidHash(m.RulesHash) || !validIdentity(m.Device) || !validIdentity(m.Generation) || !ValidPath(m.Path) {
		return ErrInvalid
	}
	switch m.Kind {
	case "replace":
		if !validFileEntry(m.After) || m.Before != nil && !validFileEntry(m.Before) {
			return ErrInvalid
		}
	case "delete":
		if !validFileEntry(m.Before) || m.After != nil {
			return ErrInvalid
		}
	case "mkdir":
		if m.Before != nil || !validDirectoryEntry(m.After) {
			return ErrInvalid
		}
	case "rmdir":
		if !validDirectoryEntry(m.Before) || m.After != nil {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
func (m Mutation) Digest() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	m.Generation = ""
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return HashBytes(data), nil
}

type MutationResult struct {
	ID       string `json:"operation_id"`
	Status   string `json:"status"` // applied or uncertain; uncertain must never be blindly retried.
	Recovery bool   `json:"recovery"`
	Replayed bool   `json:"replayed"`
}

// OperationStatus supports reconciliation and downloading the old bytes without
// reapplying a mutation. The recovery file itself is never automatically restored.
type OperationStatus struct {
	Operation              MutationResult `json:"operation"`
	Path                   string         `json:"path"`
	Kind                   string         `json:"kind"`
	Before                 *Entry         `json:"before"`
	After                  *Entry         `json:"after"`
	Digest                 string         `json:"digest,omitempty"`
	Device                 string         `json:"device,omitempty"`
	Project                string         `json:"project,omitempty"`
	Retirement             string         `json:"retirement,omitempty"`
	RetirementConfirmation string         `json:"retirement_confirmation,omitempty"`
}

func (s OperationStatus) Validate(id string) error {
	if s.Operation.ID != id || !ValidOperationID(id) || !ValidPath(s.Path) || s.Operation.Status != "applied" && s.Operation.Status != "uncertain" {
		return ErrInvalid
	}
	// Older peers omit the entire maintenance extension. New peers must provide
	// it consistently; retirement never changes the historical applied receipt.
	if s.Digest != "" || s.Device != "" || s.Project != "" || s.Retirement != "" || s.RetirementConfirmation != "" {
		if !ValidHash(s.Digest) || !validIdentity(s.Device) || !validIdentity(s.Project) || !ValidHash(s.RetirementConfirmation) || (s.Retirement != "" && s.Retirement != "retiring" && s.Retirement != "retired") || (s.Retirement != "" && (s.Operation.Status != "applied" || s.Operation.Recovery)) {
			return ErrInvalid
		}
	}
	switch s.Kind {
	case "replace":
		if validFileEntry(s.After) && (s.Before == nil || validFileEntry(s.Before)) {
			return nil
		}
	case "delete":
		if validFileEntry(s.Before) && s.After == nil {
			return nil
		}
	case "mkdir":
		if s.Before == nil && validDirectoryEntry(s.After) {
			return nil
		}
	case "rmdir":
		if validDirectoryEntry(s.Before) && s.After == nil {
			return nil
		}
	}
	return ErrInvalid
}
