package config

// Zero means unlimited/disabled. Limits apply to admission, never kill a turn.
type ResourceLimits struct {
	MaxRunning        int   `json:"max_running"`
	MaxRunningPerUser int   `json:"max_running_per_user"`
	MinFreeBytes      int64 `json:"min_free_bytes"`
}

func (c *Config) GetResources() ResourceLimits {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Resources
}
