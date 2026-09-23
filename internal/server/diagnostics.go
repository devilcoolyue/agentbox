package server

import (
	"agentbox/internal/buildinfo"
	"agentbox/internal/store"
	"net/http"
	"runtime"
)

// Allowlisted data only: never include paths, identities, config, environment,
// logs, proxy URLs, raw Docker inspect or transcripts in a shareable report.
func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	total, free := readDiskUsage(s.cfg.DataDir)
	writeJSON(w, http.StatusOK, map[string]any{
		"version": buildinfo.Version, "revision": buildinfo.Commit(), "built_at": buildinfo.BuiltAt,
		"go_version": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		"schema_version": store.SchemaVersion, "started_at": s.startedAt.UnixMilli(),
		"sessions": len(s.store.All()), "disk_total": total, "disk_available": free,
		"resources": s.cfg.GetResources(), "usage_sync": s.usageService().SyncStatus(),
	})
}
