package config

// Collaboration is opt-in while the new runtime/CLI gates are evaluated.
func (c *Config) GetCollaborationEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CollaborationEnabled
}
func (c *Config) SetCollaborationEnabled(enabled bool) error {
	return c.mutate(func(w *Config) error { w.CollaborationEnabled = enabled; return nil })
}
