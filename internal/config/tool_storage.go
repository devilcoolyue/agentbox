package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ToolStorage is operator-configured and takes effect on container recreation.
// Root holds public immutable tools and private per-user caches on a data disk.
// Empty Root disables it. It is intentionally not a remotely editable host path.
type ToolStorage struct {
	Root      string `json:"root,omitempty"`
	GoVersion string `json:"go_version,omitempty"`
}

var goToolVersion = regexp.MustCompile(`^go1\.[0-9]+\.[0-9]+$`)

func (p ToolStorage) validate(data string) error {
	if p.Root == "" {
		if p.GoVersion != "" {
			return fmt.Errorf("tool_storage.go_version requires root")
		}
		return nil
	}
	if !filepath.IsAbs(p.Root) || filepath.Clean(p.Root) != p.Root || p.Root == "/" || strings.ContainsAny(p.Root, "\x00\r\n") {
		return fmt.Errorf("tool_storage.root must be a clean absolute directory")
	}
	if p.GoVersion != "" && !goToolVersion.MatchString(p.GoVersion) {
		return fmt.Errorf("tool_storage.go_version must be an exact Go version")
	}
	users := filepath.Join(data, "users")
	if rel, err := filepath.Rel(users, p.Root); err == nil && (rel == "." || filepath.IsLocal(rel)) {
		return fmt.Errorf("tool_storage.root cannot be inside user data")
	}
	return nil
}
func (c *Config) GetToolStorage() ToolStorage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ToolStorage
}
func (c *Config) SetToolStorage(p ToolStorage) error {
	return c.mutate(func(w *Config) error { w.ToolStorage = p; return nil })
}
