package config

import "fmt"

// Cache keepalive defaults: how long after a conversation's last request its
// prompt cache is kept, the smallest prompt worth keeping, the longest a
// workspace may ask for, and how often the cache is read.
const (
	DefaultKeepaliveHours      = 3
	DefaultKeepaliveHoursLimit = 6
	DefaultKeepaliveMinTokens  = 100000
	DefaultKeepaliveInterval   = 50 // minutes
	MaxKeepaliveHours          = 24
	// The provider keeps a subscription's cache for an hour after its last
	// use; a keepalive must land well inside it (timer slack, a long last
	// response, waking the workspace).
	MinKeepaliveInterval = 30
	MaxKeepaliveInterval = 55
	// A workspace keeping caches pauses its chat once a Claude
	// subscription's five-hour window is this used, leaving the rest for the
	// keepalive (server/quota_pause.go).
	DefaultPausePercent = 95
	MinPausePercent     = 50
	MaxPausePercent     = 99
)

// CacheKeepaliveConfig is the administrator's side of the prompt cache
// keepalive (see server/keepalive.go): whether workspaces may turn it on, the
// defaults they start from, the longest they may keep a cache and how often
// it is read. Each workspace turns it on itself. Zero values take the
// defaults above.
type CacheKeepaliveConfig struct {
	// Disabled stops every workspace's keepalive and hides the switch.
	Disabled bool `json:"disabled,omitempty"`
	// MaxHours is the default for how long after a conversation's last
	// request the cache is kept.
	MaxHours int `json:"max_hours,omitempty"`
	// MaxHoursLimit caps what a workspace may set.
	MaxHoursLimit int `json:"max_hours_limit,omitempty"`
	// MinContextTokens is the default smallest prompt worth keeping.
	MinContextTokens int64 `json:"min_context_tokens,omitempty"`
	// IntervalMinutes is how long the server waits between two reads.
	IntervalMinutes int `json:"interval_minutes,omitempty"`
	// PausePercent is the default five-hour usage at which a workspace
	// keeping caches pauses its chat until the window starts over.
	PausePercent int `json:"pause_percent,omitempty"`
}

func (c *Config) validateCacheKeepalive() error {
	k := c.CacheKeepalive
	if k.MaxHours < 0 || k.MaxHours > MaxKeepaliveHours {
		return fmt.Errorf("cache_keepalive.max_hours must be between 0 and %d (0 = %d)", MaxKeepaliveHours, DefaultKeepaliveHours)
	}
	if k.MaxHoursLimit < 0 || k.MaxHoursLimit > MaxKeepaliveHours {
		return fmt.Errorf("cache_keepalive.max_hours_limit must be between 0 and %d (0 = %d)", MaxKeepaliveHours, DefaultKeepaliveHoursLimit)
	}
	if k.MaxHours > 0 && k.MaxHoursLimit > 0 && k.MaxHours > k.MaxHoursLimit {
		return fmt.Errorf("cache_keepalive.max_hours must not exceed max_hours_limit")
	}
	if k.MinContextTokens < 0 {
		return fmt.Errorf("cache_keepalive.min_context_tokens must be >= 0")
	}
	if k.IntervalMinutes != 0 && (k.IntervalMinutes < MinKeepaliveInterval || k.IntervalMinutes > MaxKeepaliveInterval) {
		return fmt.Errorf("cache_keepalive.interval_minutes must be between %d and %d (0 = %d)", MinKeepaliveInterval, MaxKeepaliveInterval, DefaultKeepaliveInterval)
	}
	if k.PausePercent != 0 && (k.PausePercent < MinPausePercent || k.PausePercent > MaxPausePercent) {
		return fmt.Errorf("cache_keepalive.pause_percent must be between %d and %d (0 = %d)", MinPausePercent, MaxPausePercent, DefaultPausePercent)
	}
	return nil
}

// GetCacheKeepalive returns the keepalive config with its defaults filled in;
// a default above the limit is lowered to it.
func (c *Config) GetCacheKeepalive() CacheKeepaliveConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := c.CacheKeepalive
	if out.MaxHoursLimit == 0 {
		out.MaxHoursLimit = DefaultKeepaliveHoursLimit
	}
	if out.MaxHours == 0 {
		out.MaxHours = DefaultKeepaliveHours
	}
	out.MaxHours = min(out.MaxHours, out.MaxHoursLimit)
	if out.MinContextTokens == 0 {
		out.MinContextTokens = DefaultKeepaliveMinTokens
	}
	if out.IntervalMinutes == 0 {
		out.IntervalMinutes = DefaultKeepaliveInterval
	}
	if out.PausePercent == 0 {
		out.PausePercent = DefaultPausePercent
	}
	return out
}
