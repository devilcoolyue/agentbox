// Package syncproto contains the portable file synchronization wire contract.
// A manifest is only valid after a complete, bounded, successful scan. Callers
// must never convert scan/transport errors into an empty manifest.
package syncproto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

const Version = 1
const DefaultMaxEntries = 100000
const DefaultMaxFileBytes int64 = 64 << 20
const DefaultMaxScanBytes int64 = 2 << 30

var ErrInvalid = errors.New("invalid sync data")
var ErrChanged = errors.New("file or directory changed during operation")
var ErrLimit = errors.New("sync limit exceeded")

type Entry struct {
	Kind       string `json:"kind"` // file or directory; links/special files are not synced.
	Hash       string `json:"hash,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Executable bool   `json:"executable,omitempty"`
}

type Manifest struct {
	Version   int              `json:"version"`
	RulesHash string           `json:"rules_hash"`
	Entries   map[string]Entry `json:"entries"`
}

func ValidPath(name string) bool {
	if name == "" || name == "." || len(name) > 4096 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00\r\n") || strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") || path.Clean(name) != name {
		return false
	}
	return true
}

func ValidHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, ch := range hash {
		if ch < '0' || ch > '9' {
			if ch < 'a' || ch > 'f' {
				return false
			}
		}
	}
	return true
}

// Validate checks structure including parent directories, not just individual
// paths. Otherwise a file named a and another named a/b could bypass preflight.
func (m Manifest) Validate() error {
	if m.Version != Version || !ValidHash(m.RulesHash) || m.Entries == nil || len(m.Entries) > DefaultMaxEntries {
		return ErrInvalid
	}
	for name, entry := range m.Entries {
		if !ValidPath(name) {
			return fmt.Errorf("%w: path", ErrInvalid)
		}
		switch entry.Kind {
		case "directory":
			if entry.Hash != "" || entry.Size != 0 || entry.Executable {
				return fmt.Errorf("%w: directory metadata", ErrInvalid)
			}
		case "file":
			if !ValidHash(entry.Hash) || entry.Size < 0 || entry.Size > DefaultMaxFileBytes {
				return fmt.Errorf("%w: file metadata", ErrInvalid)
			}
		default:
			return fmt.Errorf("%w: file type", ErrInvalid)
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if m.Entries[parent].Kind != "directory" {
				return fmt.Errorf("%w: missing directory", ErrInvalid)
			}
		}
	}
	return nil
}

func HashBytes(bytes []byte) string { sum := sha256.Sum256(bytes); return hex.EncodeToString(sum[:]) }

// Digest's map key order is deterministic in encoding/json. Bind preview
// confirmation to this digest; a rescanned tree must produce a new preview.
func (m Manifest) Digest() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return HashBytes(data), nil
}

type Binding struct {
	Version     int    `json:"version"`
	Server      string `json:"server"` // normalized base URL, never bearer credentials.
	ServerID    string `json:"server_id"`
	User        string `json:"user"`
	Workspace   string `json:"workspace"`
	Project     string `json:"project"`
	ProjectPath string `json:"project_path"`
	LocalID     string `json:"local_id"` // pinned directory identity, not just a pathname.
}

type Baseline struct {
	Binding Binding  `json:"binding"`
	Local   Manifest `json:"local"`
	Remote  Manifest `json:"remote"`
}
