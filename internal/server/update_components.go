package server

import (
	"context"
	"net/http"
	"time"

	"agentbox/internal/buildinfo"
	"agentbox/internal/imageupdate"
	"agentbox/internal/store"
)

type componentImageInspector interface {
	InspectCLIImage(context.Context, string) (imageupdate.Image, error)
}

type updateComponentsView struct {
	Version    int   `json:"version"`
	ObservedAt int64 `json:"observed_at"`
	Server     struct {
		Version   string `json:"version"`
		Schema    int    `json:"schema"`
		Candidate string `json:"candidate_compatibility"`
	} `json:"server"`
	Image struct {
		Reference  string `json:"reference"`
		ID         string `json:"id"`
		Claude     string `json:"claude"`
		Codex      string `json:"codex"`
		State      string `json:"state"`
		ObservedAt int64  `json:"observed_at"`
	} `json:"image"`
	Desktop struct {
		Installed string `json:"installed_version_state"`
		Protocol  int    `json:"server_protocol"`
		Sync      bool   `json:"sync_enabled"`
	} `json:"desktop"`
}

// This observation never contacts release registries, runs a CLI, pulls an
// image, starts a container or changes update policy. Image labels are metadata,
// not a protocol test or proof about already-running workspace containers.
func (s *Server) updateComponents(ctx context.Context, inspector componentImageInspector) updateComponentsView {
	var v updateComponentsView
	v.Version = 1
	v.ObservedAt = time.Now().UnixMilli()
	v.Server.Version, v.Server.Schema = buildinfo.Version, store.SchemaVersion
	v.Server.Candidate = "preflight_required"
	v.Desktop.Installed, v.Desktop.Protocol = "browser_unknown", 1
	v.Desktop.Sync = s.cfg.GetDesktopSyncEnabled()
	v.Image.Reference, v.Image.State = s.cfg.GetAgentImage(), "unavailable"
	if inspector == nil {
		return v
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	img, err := inspector.InspectCLIImage(ctx, v.Image.Reference)
	if err != nil {
		return v // Host paths and daemon errors do not belong in the overview.
	}
	if s.cfg.GetAgentImage() != v.Image.Reference {
		v.Image.Reference, v.Image.State = s.cfg.GetAgentImage(), "changed"
		return v
	}
	v.Image.State = "labels_only"
	v.Image.ID, v.Image.Claude, v.Image.Codex = img.ID, img.Claude, img.Codex
	v.Image.ObservedAt = time.Now().UnixMilli()
	return v
}

func (s *Server) handleUpdateComponents(w http.ResponseWriter, r *http.Request) {
	var inspector componentImageInspector
	if s.dock != nil {
		inspector = s.dock
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.updateComponents(r.Context(), inspector))
}
