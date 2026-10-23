package server

import (
	"agentbox/internal/safefs"
	"agentbox/internal/storagepolicy"
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type storageReport struct {
	ScannedAt  int64            `json:"scanned_at"`
	Partial    bool             `json:"partial"`
	Bytes      int64            `json:"bytes"`
	Files      int              `json:"files"`
	Users      map[string]int64 `json:"users"`
	Sessions   map[string]int64 `json:"sessions"` // users/<u>/sessions/<id> by session id
	Shared     map[string]int64 `json:"shared"`   // users/<u>/shared by user
	Categories map[string]int64 `json:"categories"`
}
type storageState struct {
	sync.RWMutex
	report   storageReport
	quota    storagepolicy.Report
	quotaAt  int64 // ms; 0 until the first successful read
	quotaErr string
}

// Walk bounds: the production data dir already holds ~150k entries, so the
// old 100k cap left every report partial. One walk every five minutes.
const (
	storageWalkEntries = 1_000_000
	storageWalkTimeout = time.Minute
)

// Logical file lengths, not allocated blocks: hardlinks count twice; symlinks
// are never followed. Reports are bounded and explicitly marked incomplete.
func measureStorage(ctx context.Context, dir string) storageReport {
	report := storageReport{Users: map[string]int64{}, Sessions: map[string]int64{}, Shared: map[string]int64{}, Categories: map[string]int64{}}
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
		if entries > storageWalkEntries {
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
		category := strings.Split(path, "/")[0]
		if category != "users" && category != "backups" {
			category = "other"
		}
		report.Categories[category] += info.Size()
		report.Files++
		parts := strings.Split(path, "/")
		if len(parts) >= 3 && parts[0] == "users" {
			report.Users[parts[1]] += info.Size()
			if parts[2] == "shared" {
				report.Shared[parts[1]] += info.Size()
			} else if parts[2] == "sessions" && len(parts) >= 5 {
				report.Sessions[parts[3]] += info.Size()
			}
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
		ctx, cancel := context.WithTimeout(s.workContext(), storageWalkTimeout)
		report := measureStorage(ctx, s.cfg.DataDir)
		cancel()
		tools := s.cfg.GetToolStorage().Root
		if tools != "" {
			for _, kind := range []string{"public", "private"} {
				scanCtx, done := context.WithTimeout(s.workContext(), 10*time.Second)
				measured := measureStorage(scanCtx, filepath.Join(tools, kind))
				done()
				report.Categories["tools_"+kind] = measured.Bytes
				report.Partial = report.Partial || measured.Partial
			}
		}
		if exe, err := os.Executable(); err == nil {
			releases := filepath.Dir(filepath.Dir(exe))
			if filepath.Base(releases) == "releases" {
				scanCtx, done := context.WithTimeout(s.workContext(), 10*time.Second)
				measured := measureStorage(scanCtx, releases)
				done()
				report.Categories["releases"] = measured.Bytes
				report.Partial = report.Partial || measured.Partial
			}
		}
		s.storage.Lock()
		s.storage.report = report
		s.storage.Unlock()
		if !waitInterval(s.workContext(), 5*time.Minute) {
			return
		}
	}
}

// storageQuotaLoop keeps the kernel quota usage of every user fresh for the
// monitor. quotactl itself is instant; the minute only spares a python start
// on every five-second monitor poll.
func (s *Server) storageQuotaLoop() {
	for s.workContext().Err() == nil {
		if p := s.cfg.GetStoragePolicy(); p.Mount != "" {
			ctx, cancel := context.WithTimeout(s.workContext(), 15*time.Second)
			report, err := storagepolicy.ReadReport(ctx, p, s.cfg.DataDir)
			cancel()
			s.storage.Lock()
			if err != nil {
				s.storage.quotaErr = err.Error()
			} else {
				s.storage.quota, s.storage.quotaAt, s.storage.quotaErr = report, time.Now().UnixMilli(), ""
			}
			s.storage.Unlock()
			if err != nil && s.workContext().Err() == nil {
				log.Printf("storage: read quota usage: %v", err)
			}
		}
		if !waitInterval(s.workContext(), time.Minute) {
			return
		}
	}
}

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	s.storage.RLock()
	report := s.storage.report
	s.storage.RUnlock()
	total, free := readDiskUsage(s.cfg.DataDir)
	writeJSON(w, http.StatusOK, map[string]any{"data": report, "disk_total": total, "disk_available": free, "disks": s.storageDisks(), "storage_policy": s.cfg.GetStoragePolicy()})
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
