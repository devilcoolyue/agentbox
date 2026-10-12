package collaboration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"agentbox/internal/safefs"
)

const MaxReviewEvidenceBytes = 192 << 10

// Bounds are frozen into the evidence, so absence of a diff is distinguishable
// from absence of a change. Unlisted paths remain counted and covered by the
// digest of the complete manifest comparison.
type ReviewEvidenceLimits struct {
	MaxChanges       int   `json:"max_changes"`
	MaxFileBytes     int64 `json:"max_file_bytes"`
	MaxFileDiffBytes int   `json:"max_file_diff_bytes"`
	MaxDiffBytes     int   `json:"max_diff_bytes"`
	MaxBytes         int   `json:"max_bytes"`
}

var reviewEvidenceLimits = ReviewEvidenceLimits{MaxChanges: 256, MaxFileBytes: 256 << 10, MaxFileDiffBytes: 24 << 10, MaxDiffBytes: 64 << 10, MaxBytes: MaxReviewEvidenceBytes}

type ReviewFileEvidence struct {
	FileChange
	Diff          string `json:"diff,omitempty"`
	Hunks         int    `json:"hunks"`
	IncludedHunks int    `json:"included_hunks"`
	Complete      bool   `json:"complete"`
	Omission      string `json:"omission,omitempty"`
}

type ReviewEvidence struct {
	Version         int                  `json:"version"`
	BaselineID      string               `json:"baseline_id"`
	BaselineSHA256  string               `json:"baseline_sha256"`
	SnapshotID      string               `json:"snapshot_id"`
	SnapshotSHA256  string               `json:"snapshot_sha256"`
	PlanVersion     int64                `json:"plan_version"`
	ChangesSHA256   string               `json:"changes_sha256"`
	ChangesTotal    int                  `json:"changes_total"`
	OmittedChanges  int                  `json:"omitted_changes"`
	Changes         []ReviewFileEvidence `json:"changes"`
	Complete        bool                 `json:"complete"`
	OmissionReasons []string             `json:"omission_reasons,omitempty"`
	Limits          ReviewEvidenceLimits `json:"limits"`
}

func reviewDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validOmission(reason string) bool {
	switch reason {
	case "binary", "file_size", "diff_complexity", "diff_budget", "inventory_budget":
		return true
	}
	return false
}

// Validate checks the frozen data and binding; callers must ALSO match the two
// manifest digests against their persisted artifacts before first accepting it.
func (e ReviewEvidence) Validate(snapshot string, planVersion int64) error {
	if e.Version != 1 || !ValidID(e.BaselineID) || !ValidID(e.SnapshotID) || e.SnapshotID != snapshot ||
		e.PlanVersion != planVersion || planVersion < 1 || !validDigest(e.BaselineSHA256) || !validDigest(e.SnapshotSHA256) || !validDigest(e.ChangesSHA256) ||
		e.Limits != reviewEvidenceLimits || e.ChangesTotal < 0 || e.ChangesTotal > 200000 || e.OmittedChanges < 0 ||
		e.ChangesTotal != len(e.Changes)+e.OmittedChanges || len(e.Changes) > e.Limits.MaxChanges {
		return ErrInvalid
	}
	complete := e.OmittedChanges == 0
	reasons := map[string]bool{}
	if e.OmittedChanges != 0 {
		reasons["inventory_budget"] = true
	}
	changes := make([]FileChange, 0, len(e.Changes))
	last, diffBytes := "", 0
	for _, change := range e.Changes {
		if !validProjectPath(change.Path) || change.Path <= last || change.Hunks < 0 || change.IncludedHunks < 0 || change.IncludedHunks > change.Hunks ||
			change.Complete != (change.Omission == "") || (!change.Complete && !validOmission(change.Omission)) ||
			!utf8.ValidString(change.Diff) || strings.ContainsRune(change.Diff, 0) || len(change.Diff) > e.Limits.MaxFileDiffBytes ||
			(change.Complete && change.Hunks != change.IncludedHunks) || (change.IncludedHunks == 0) != (change.Diff == "") {
			return ErrInvalid
		}
		switch change.Status {
		case "added":
			if change.Before != nil || change.After == nil {
				return ErrInvalid
			}
		case "deleted":
			if change.Before == nil || change.After != nil {
				return ErrInvalid
			}
		case "modified":
			if change.Before == nil || change.After == nil || *change.Before == *change.After {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
		for _, entry := range []*SnapshotEntry{change.Before, change.After} {
			if entry == nil {
				continue
			}
			if entry.Path != change.Path || entry.Mode > 0777 {
				return ErrInvalid
			}
			switch entry.Kind {
			case "file":
				if entry.Size < 0 || !validDigest(entry.SHA256) || entry.Target != "" {
					return ErrInvalid
				}
			case "directory":
				if entry.Size != 0 || entry.SHA256 != "" || entry.Target != "" {
					return ErrInvalid
				}
			case "symlink":
				if entry.Size != 0 || entry.SHA256 != "" || !validText(entry.Target, 4096) {
					return ErrInvalid
				}
			default:
				return ErrInvalid
			}
		}
		if !change.Complete {
			complete = false
			reasons[change.Omission] = true
		}
		last = change.Path
		diffBytes += len(change.Diff)
		changes = append(changes, change.FileChange)
	}
	if complete != e.Complete || diffBytes > e.Limits.MaxDiffBytes || len(reasons) != len(e.OmissionReasons) {
		return ErrInvalid
	}
	for _, reason := range e.OmissionReasons {
		if !reasons[reason] {
			return ErrInvalid
		}
		delete(reasons, reason)
	}
	if e.OmittedChanges == 0 {
		raw, err := json.Marshal(changes)
		if err != nil || reviewDigest(raw) != e.ChangesSHA256 {
			return ErrInvalid
		}
	}
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > MaxReviewEvidenceBytes {
		return ErrInvalid
	}
	return nil
}

// BuildReviewEvidence requires two VerifySnapshot results from the same pinned
// snapshots directory. File bytes are bounded and rehashed after reading; no
// Git process, working tree, raw provider transcript or link target is read.
func BuildReviewEvidence(ctx context.Context, snapshots *safefs.Root, before, after Snapshot, planVersion int64) (ReviewEvidence, error) {
	e := ReviewEvidence{Version: 1, BaselineID: before.ID, BaselineSHA256: before.ManifestSHA256, SnapshotID: after.ID, SnapshotSHA256: after.ManifestSHA256, PlanVersion: planVersion, Complete: true, Changes: []ReviewFileEvidence{}, Limits: reviewEvidenceLimits}
	if !ValidID(before.ID) || !ValidID(after.ID) || !validDigest(before.ManifestSHA256) || !validDigest(after.ManifestSHA256) || planVersion < 1 {
		return e, ErrInvalid
	}
	changes := SnapshotChanges(before.Manifest, after.Manifest)
	raw, err := json.Marshal(changes)
	if err != nil {
		return e, err
	}
	e.ChangesSHA256, e.ChangesTotal = reviewDigest(raw), len(changes)
	reasons := map[string]bool{}
	// Reserve ample room for the envelope and escaping in metadata. Each item's
	// encoded size is counted, not just the number of diff text bytes.
	bytesLeft, diffLeft := e.Limits.MaxBytes-4096, e.Limits.MaxDiffBytes
	for i, change := range changes {
		if err := ctx.Err(); err != nil {
			return e, err
		}
		if len(e.Changes) >= e.Limits.MaxChanges {
			e.OmittedChanges = len(changes) - i
			break
		}
		item := ReviewFileEvidence{FileChange: change, Complete: true}
		// A mode-only change is fully represented by metadata, even for a large
		// or binary file, and need not consume a read or a textual diff budget.
		contentSame := change.Before != nil && change.After != nil && change.Before.Kind == change.After.Kind && change.Before.SHA256 == change.After.SHA256 && change.Before.Target == change.After.Target
		if !contentSame {
			oversized := (change.Before != nil && change.Before.Size > e.Limits.MaxFileBytes) || (change.After != nil && change.After.Size > e.Limits.MaxFileBytes)
			if oversized {
				item.Omission = "file_size"
			} else {
				old, err := readReviewEntry(ctx, snapshots, before.ID, change.Before)
				if err != nil {
					return e, err
				}
				current, err := readReviewEntry(ctx, snapshots, after.ID, change.After)
				if err != nil {
					return e, err
				}
				diff, err := compactReviewDiff(ctx, change.Path, old, current, min(e.Limits.MaxFileDiffBytes, diffLeft))
				if err != nil {
					return e, err
				}
				item.Diff, item.Hunks, item.IncludedHunks, item.Omission = diff.Text, diff.Hunks, diff.IncludedHunks, diff.Omission
			}
		}
		item.Complete = item.Omission == ""
		encoded, err := json.Marshal(item)
		if err != nil {
			return e, err
		}
		if len(encoded)+1 > bytesLeft {
			// Keep this path in the inventory if dropping its diff makes room.
			if item.Diff != "" {
				item.Diff, item.IncludedHunks, item.Complete, item.Omission = "", 0, false, "diff_budget"
				encoded, _ = json.Marshal(item)
			}
			if len(encoded)+1 > bytesLeft {
				e.OmittedChanges = len(changes) - i
				break
			}
		}
		bytesLeft -= len(encoded) + 1
		diffLeft -= len(item.Diff)
		e.Changes = append(e.Changes, item)
		if !item.Complete {
			reasons[item.Omission] = true
		}
	}
	if e.OmittedChanges != 0 {
		reasons["inventory_budget"] = true
	}
	for reason := range reasons {
		e.OmissionReasons = append(e.OmissionReasons, reason)
	}
	sort.Strings(e.OmissionReasons)
	e.Complete = len(reasons) == 0
	return e, e.Validate(after.ID, planVersion)
}

func readReviewEntry(ctx context.Context, snapshots *safefs.Root, id string, entry *SnapshotEntry) (string, error) {
	if entry == nil || entry.Kind == "directory" {
		return "", nil
	}
	if !validProjectPath(entry.Path) {
		return "", ErrInvalid
	}
	if entry.Kind == "symlink" {
		return entry.Target, nil
	}
	if entry.Kind != "file" || entry.Size < 0 || entry.Size > reviewEvidenceLimits.MaxFileBytes {
		return "", ErrInvalid
	}
	f, err := snapshots.OpenFile(id + "/tree/" + entry.Path)
	if err != nil {
		return "", err
	}
	raw, readErr := io.ReadAll(io.LimitReader(&snapshotReader{ctx: ctx, r: f}, entry.Size+1))
	if err = errors.Join(readErr, f.Close()); err != nil {
		return "", err
	}
	if int64(len(raw)) != entry.Size || reviewDigest(raw) != entry.SHA256 {
		return "", errors.New("review snapshot content changed")
	}
	return string(raw), nil
}
