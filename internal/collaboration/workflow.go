package collaboration

import (
	"path"
	"strings"
	"unicode"
)

// Workflow is explicitly versioned. An absent workflow keeps legacy requests
// and their saved step inputs compatible; direct skips model planning/review,
// never the platform's verification and delivery checks.
type Workflow struct {
	Version int    `json:"version"`
	Mode    string `json:"mode"`
}

func (w Workflow) Validate() error {
	if w.Version != 1 || (w.Mode != "direct" && w.Mode != "reviewed") {
		return ErrInvalid
	}
	return nil
}

func (w *Workflow) IsDirect() bool { return w != nil && w.Version == 1 && w.Mode == "direct" }

func validCodeMap(entries []CodeMapEntry) bool {
	if len(entries) > 32 {
		return false
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !validCodeMapPath(e.Path) || seen[e.Path] || !validText(e.Purpose, 1024) ||
			!boundedTexts(e.Symbols, 16, 256) || !boundedTexts(e.Constraints, 8, 1024) ||
			(e.Excerpt != "" && !validText(e.Excerpt, 2048)) {
			return false
		}
		seen[e.Path] = true
	}
	return true
}

func validCodeMapPath(name string) bool {
	if !validText(name, 512) || path.IsAbs(name) || path.Clean(name) != name || name == "." ||
		strings.ContainsAny(name, "\\:") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".git" || part == ".." {
			return false
		}
	}
	return true
}

func boundedTexts(values []string, count, size int) bool {
	if len(values) > count {
		return false
	}
	for _, value := range values {
		if !validText(value, size) {
			return false
		}
	}
	return true
}
