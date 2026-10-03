package syncclient

import (
	"context"
	"errors"
	"io"
	"os"

	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

type Inspection struct {
	Capabilities   syncfs.Capabilities `json:"capabilities"`
	Files          int                 `json:"files"`
	Directories    int                 `json:"directories"`
	Bytes          int64               `json:"bytes"`
	ManifestDigest string              `json:"manifest_digest"`
	RulesHash      string              `json:"rules_hash"`
	WindowsIssues  int                 `json:"windows_issues"`
}

// InspectLocal opens a user-selected directory, probes its volume, reads the
// project's optional ignore file and performs a complete bounded scan. Only a
// small summary crosses IPC; raw file bytes and names are never logged.
func InspectLocal(ctx context.Context, directory string) (Inspection, error) {
	root, err := syncfs.OpenContext(ctx, directory)
	if err != nil {
		return Inspection{}, err
	}
	defer root.Close()
	capabilities, err := root.Probe()
	if err != nil {
		return Inspection{}, err
	}
	var text []byte
	f, err := root.OpenFile(".agentboxignore")
	if err == nil {
		text, err = io.ReadAll(io.LimitReader(f, (64<<10)+1))
		f.Close()
		if err != nil {
			return Inspection{}, err
		}
	} else if !os.IsNotExist(err) {
		return Inspection{}, err
	}
	rules, err := syncproto.ParseRules(string(text))
	if err != nil {
		return Inspection{}, err
	}
	manifest, err := root.Scan(ctx, rules, syncfs.Limits{})
	if err != nil {
		return Inspection{}, err
	}
	if err = syncfs.RequireNames(manifest, capabilities.NamePolicy); err != nil {
		return Inspection{}, err
	}
	// The ignore file was read before scanning. It must still be the same file
	// content even when a user rule excludes it from the returned manifest.
	again, readErr := root.OpenFile(".agentboxignore")
	if readErr == nil {
		current, err := io.ReadAll(io.LimitReader(again, (64<<10)+1))
		again.Close()
		if err != nil {
			return Inspection{}, err
		}
		if string(current) != string(text) {
			return Inspection{}, syncproto.ErrChanged
		}
	} else if !os.IsNotExist(readErr) || text != nil {
		return Inspection{}, syncproto.ErrChanged
	}
	issues, err := syncfs.CheckNames(manifest, syncfs.NamePolicy{Windows: true, CaseSensitive: false, NormalizationSensitive: false, MaxPathUnits: 200})
	if err != nil {
		return Inspection{}, err
	}
	result := Inspection{Capabilities: capabilities, RulesHash: rules.Hash(), WindowsIssues: len(issues)}
	result.ManifestDigest, err = manifest.Digest()
	if err != nil {
		return Inspection{}, err
	}
	for _, entry := range manifest.Entries {
		if entry.Kind == "file" {
			result.Files++
			result.Bytes += entry.Size
		} else {
			result.Directories++
		}
	}
	return result, nil
}

func inspectionError(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "inspection_canceled"
	case errors.Is(err, syncfs.ErrUnsupportedVolume):
		return "inspection_unsupported_volume"
	case errors.Is(err, syncproto.ErrLimit):
		return "inspection_limit"
	case errors.Is(err, syncproto.ErrChanged), errors.Is(err, syncfs.ErrRootChanged):
		return "inspection_changed"
	case errors.Is(err, syncproto.ErrInvalid), errors.Is(err, syncfs.ErrUnsafe):
		return "inspection_unsafe"
	default:
		return "inspection_io"
	}
}
