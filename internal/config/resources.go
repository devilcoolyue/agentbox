package config

// Zero means unlimited/disabled. Limits apply to admission, never kill a turn.
type ResourceLimits struct {
	MaxRunning        int   `json:"max_running"`
	MaxRunningPerUser int   `json:"max_running_per_user"`
	MinFreeBytes      int64 `json:"min_free_bytes"`
}

// routineFloorBytes keeps room for SQLite and session state once routine work
// has been allowed to eat into the reserve.
const routineFloorBytes = 1 << 30

// FloorBytes is the free space below which routine work is refused as well.
// MinFreeBytes only gates what grows the disk: new workspaces, uploads, imports.
func (r ResourceLimits) FloorBytes() int64 { return min(r.MinFreeBytes, routineFloorBytes) }

// LowWaterBytes is where the operator is warned, while nothing is refused yet.
func (r ResourceLimits) LowWaterBytes() int64 { return r.MinFreeBytes * 2 }

func (c *Config) GetResources() ResourceLimits {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Resources
}
