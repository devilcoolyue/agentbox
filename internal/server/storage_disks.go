package server

import (
	"os"
	"syscall"
)

type storageDisk struct {
	Roles     []string `json:"roles"`
	Total     uint64   `json:"total"`
	Used      uint64   `json:"used"`
	Available uint64   `json:"available"`
	Error     bool     `json:"error,omitempty"`
}

// Roles share one entry when backed by the same filesystem. Do not add logical
// file sizes to physical disk use (hardlinks/reflinks can share physical blocks).
func (s *Server) storageDisks() []storageDisk {
	roots := []struct{ role, path string }{{"system", "/"}, {"data", s.cfg.DataDir}, {"tools", s.cfg.GetToolStorage().Root}}
	seen := map[uint64]int{}
	result := []storageDisk{}
	for _, root := range roots {
		if root.path == "" {
			continue
		}
		info, err := os.Stat(root.path)
		if err != nil {
			result = append(result, storageDisk{Roles: []string{root.role}, Error: true})
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		dev := uint64(stat.Dev)
		if i, ok := seen[dev]; ok {
			result[i].Roles = append(result[i].Roles, root.role)
			continue
		}
		total, free := readDiskUsage(root.path)
		seen[dev] = len(result)
		result = append(result, storageDisk{Roles: []string{root.role}, Total: total, Used: total - free, Available: free, Error: total == 0})
	}
	return result
}
