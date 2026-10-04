package syncproto

import (
	"fmt"
	"path"
	"strings"
)

// Rules deliberately implement a documented subset, not a misleading partial
// .gitignore parser. No negation: a reserved state directory is always ignored.
type Rules struct {
	patterns []string
	hash     string
}

var defaultIgnored = map[string]bool{".git": true, "node_modules": true, ".venv": true, "venv": true, "__pycache__": true, "dist": true, "build": true, "target": true, ".agentbox-sync": true}

func ParseRules(text string) (Rules, error) {
	if len(text) > 64<<10 {
		return Rules{}, ErrLimit
	}
	var patterns []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(patterns) >= 256 || len(line) > 1024 {
			return Rules{}, ErrLimit
		}
		if strings.HasPrefix(line, "!") || strings.ContainsAny(line, "\\\x00\r") || strings.Contains(line, "//") {
			return Rules{}, fmt.Errorf("%w: unsupported ignore pattern", ErrInvalid)
		}
		line = strings.TrimSuffix(strings.TrimPrefix(line, "/"), "/")
		if line == "" {
			return Rules{}, ErrInvalid
		}
		for _, part := range strings.Split(line, "/") {
			if part == "." || part == ".." || part != "**" && strings.Contains(part, "**") {
				return Rules{}, ErrInvalid
			}
			if _, err := path.Match(part, ""); err != nil {
				return Rules{}, fmt.Errorf("%w: ignore glob", ErrInvalid)
			}
		}
		patterns = append(patterns, line)
	}
	return Rules{patterns: patterns, hash: HashBytes([]byte("agentbox-ignore-v1\n" + strings.Join(patterns, "\n")))}, nil
}
func (r Rules) Hash() string { return r.hash }
func (r Rules) Ignored(name string) bool {
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if defaultIgnored[part] || strings.HasPrefix(part, ".agentbox-write-") || strings.HasPrefix(part, ".agentbox-sync-tmp-") {
			return true
		}
	}
	for _, pattern := range r.patterns {
		if !strings.Contains(pattern, "/") {
			for _, part := range parts {
				if matched, _ := path.Match(pattern, part); matched {
					return true
				}
			}
		} else {
			patternParts := strings.Split(pattern, "/")
			// A directory match suppresses its complete subtree as well.
			for n := 1; n <= len(parts); n++ {
				if matchParts(patternParts, parts[:n]) {
					return true
				}
			}
		}
	}
	return false
}

func matchParts(pattern, parts []string) bool {
	// Dynamic programming keeps user-supplied ** patterns bounded, without
	// exponential recursion on repeated wildcard segments.
	row := make([]bool, len(parts)+1)
	row[0] = true
	for _, segment := range pattern {
		next := make([]bool, len(parts)+1)
		if segment == "**" {
			next[0] = row[0]
			for j := 1; j <= len(parts); j++ {
				next[j] = row[j] || next[j-1]
			}
		} else {
			for j := 1; j <= len(parts); j++ {
				matched, _ := path.Match(segment, parts[j-1])
				next[j] = row[j-1] && matched
			}
		}
		row = next
	}
	return row[len(parts)]
}
