package server

import (
	"agentbox/internal/safefs"
	"context"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type storageReport struct {
	ScannedAt int64            `json:"scanned_at"`
	Partial   bool             `json:"partial"`
	Bytes     int64            `json:"bytes"`
	Files     int              `json:"files"`
	Users     map[string]int64 `json:"users"`
}
type storageState struct {
	sync.RWMutex
	report storageReport
}

// Logical file lengths, not allocated blocks: hardlinks count twice; symlinks
// are never followed. Reports are bounded and explicitly marked incomplete.
func measureStorage(ctx context.Context, dir string) storageReport {
	report := storageReport{Users: map[string]int64{}}
	root, err := safefs.Open(dir)
	if err != nil {
		report.Partial = true
		return report
	}
	defer root.Close()
	entries := 0
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		entries++
		if entries > 100000 {
			report.Partial = true
			return fs.SkipAll
		}
		if err != nil {
			report.Partial = true
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			report.Partial = true
			return nil
		}
		report.Bytes += info.Size()
		report.Files++
		parts := strings.Split(path, "/")
		if len(parts) >= 3 && parts[0] == "users" {
			report.Users[parts[1]] += info.Size()
		}
		return nil
	})
	if err != nil {
		report.Partial = true
	}
	report.ScannedAt = time.Now().UnixMilli()
	return report
}
func (s *Server) storageLoop() {
	for s.workContext().Err() == nil {
		ctx, cancel := context.WithTimeout(s.workContext(), 10*time.Second)
		report := measureStorage(ctx, s.cfg.DataDir)
		cancel()
		s.storage.Lock()
		s.storage.report = report
		s.storage.Unlock()
		if !waitInterval(s.workContext(), 5*time.Minute) {
			return
		}
	}
}
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	s.storage.RLock()
	report := s.storage.report
	s.storage.RUnlock()
	total, free := readDiskUsage(s.cfg.DataDir)
	writeJSON(w, http.StatusOK, map[string]any{"data": report, "disk_total": total, "disk_available": free})
}
func (s *Server) handleClearMarketCache(w http.ResponseWriter, r *http.Request) {
	s.marketMu.Lock()
	defer s.marketMu.Unlock()
	root, err := safefs.Open(filepath.Dir(s.marketDir()))
	if err == nil {
		defer root.Close()
		err = root.RemoveAll("marketplace")
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "清理市场缓存失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cleared": true})
}
