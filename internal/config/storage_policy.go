package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
)

// StoragePolicy is configured by the operator and applied after a restart.
// Project quotas require a prepared Linux filesystem; zero values disable it.
type StoragePolicy struct {
	CacheUsers       map[string]int64 `json:"cache_users,omitempty"`
	BudgetBytes      int64            `json:"budget_bytes,omitempty"`
	Mount            string           `json:"mount,omitempty"`
	UUID             string           `json:"uuid,omitempty"`
	UserBytes        int64            `json:"user_bytes,omitempty"`
	CacheBytes       int64            `json:"cache_bytes,omitempty"`
	Users            map[string]int64 `json:"users,omitempty"`
	CacheMaxAgeDays  int              `json:"cache_max_age_days,omitempty"`
	CacheTargetBytes int64            `json:"cache_target_bytes,omitempty"`
	ReleaseKeep      int              `json:"release_keep,omitempty"`
}

func (p StoragePolicy) validate(data, tools string) error {
	if p.BudgetBytes < 0 || p.UserBytes < 0 || p.CacheBytes < 0 || p.CacheTargetBytes < 0 || p.CacheMaxAgeDays < 0 || p.ReleaseKeep < 0 || p.ReleaseKeep == 1 {
		return fmt.Errorf("invalid storage policy limits (release_keep is 0 or >=2)")
	}
	if p.Mount == "" {
		if p.UUID != "" || p.UserBytes != 0 || p.CacheBytes != 0 || len(p.Users) != 0 || len(p.CacheUsers) != 0 || p.BudgetBytes != 0 {
			return fmt.Errorf("storage quotas require mount and uuid")
		}
		return nil
	}
	if !filepath.IsAbs(p.Mount) || filepath.Clean(p.Mount) != p.Mount || p.Mount == "/" || !regexp.MustCompile(`^[a-fA-F0-9-]{16,40}$`).MatchString(p.UUID) {
		return fmt.Errorf("storage policy requires a dedicated absolute mount and filesystem UUID")
	}
	for _, root := range []string{data, tools} {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(p.Mount, root)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			return fmt.Errorf("data and tool storage must be below storage_policy.mount")
		}
	}
	if p.UserBytes < 1<<20 || (tools != "" && p.CacheBytes < 1<<20) {
		return fmt.Errorf("enabled quotas require positive data/cache limits >=1 MiB")
	}
	for user, limit := range p.Users {
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`).MatchString(user) || limit < 1<<20 {
			return fmt.Errorf("invalid per-user storage limit")
		}
	}
	for user, limit := range p.CacheUsers {
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`).MatchString(user) || limit < 1<<20 {
			return fmt.Errorf("invalid per-user cache limit")
		}
	}
	if p.CacheBytes > 0 && p.CacheTargetBytes >= p.CacheBytes {
		return fmt.Errorf("cache target must be below hard quota")
	}
	return nil
}
func (p StoragePolicy) Clone() StoragePolicy {
	p.Users = maps.Clone(p.Users)
	p.CacheUsers = maps.Clone(p.CacheUsers)
	return p
}
func (c *Config) GetStoragePolicy() StoragePolicy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.StoragePolicy.Clone()
}
func (c *Config) SetStoragePolicy(p StoragePolicy) error {
	return c.mutate(func(w *Config) error { w.StoragePolicy = p.Clone(); return nil })
}

// SetUserStorageLimits sets one user's data/cache limits; 0 falls back to the
// policy defaults. The kernel limits are applied by the caller.
func (c *Config) SetUserStorageLimits(user string, data, cache int64) error {
	return c.mutate(func(w *Config) error {
		set := func(m map[string]int64, v int64) map[string]int64 {
			if v <= 0 {
				delete(m, user)
				return m
			}
			if m == nil {
				m = map[string]int64{}
			}
			m[user] = v
			return m
		}
		w.StoragePolicy.Users = set(w.StoragePolicy.Users, data)
		w.StoragePolicy.CacheUsers = set(w.StoragePolicy.CacheUsers, cache)
		return nil
	})
}

// RelocateStorage is an offline operator operation after data has been copied
// and verified. It does not copy files or hot-switch the running server.
func (c *Config) RelocateStorage(destination string, policy StoragePolicy, reserve int64) error {
	return c.mutate(func(w *Config) error {
		if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
			return fmt.Errorf("clean absolute data destination required")
		}
		old := w.DataDir
		move := func(path string) string {
			rel, err := filepath.Rel(old, path)
			if err == nil && (rel == "." || filepath.IsLocal(rel)) {
				return filepath.Join(destination, rel)
			}
			return path
		}
		w.DataDir = destination
		w.rawDataDir = destination
		w.CacheDir = move(w.CacheDir)
		w.rawCacheDir = w.CacheDir
		for i := range w.Accounts {
			if w.Accounts[i].CredentialsDir != "" {
				w.Accounts[i].CredentialsDir = move(w.Accounts[i].CredentialsDir)
				w.Accounts[i].rawCredDir = w.Accounts[i].CredentialsDir
			}
		}
		w.StoragePolicy = policy.Clone()
		w.Resources.MinFreeBytes = reserve
		return nil
	})
}
