package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"agentbox/internal/imageupdate"
)

func (s *Server) imageUpdater() *imageupdate.Service {
	s.imageUpdatesOnce.Do(func() { s.imageUpdates = imageupdate.New(s.cfg, s.dock) })
	return s.imageUpdates
}
func (s *Server) handleImageUpdates(w http.ResponseWriter, r *http.Request) {
	policy, current, previous := s.cfg.ImageUpdateState()
	writeJSON(w, 200, map[string]any{"settings": policy, "agent_image": current, "previous_image": previous, "timezone": s.cfg.GetTimeZone(), "status": s.imageUpdater().Snapshot()})
}
func (s *Server) startImageUpdate(action string, scheduled bool) error {
	run, err := s.imageUpdater().Prepare(action, scheduled)
	if err != nil || run == nil {
		return err
	}
	if !s.spawn(func() { run(s.workContext()) }) {
		ctx, cancel := context.WithCancel(s.workContext())
		cancel()
		run(ctx)
	}
	return nil
}
func (s *Server) handleImageUpdateAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "check" && action != "update" && action != "rollback" {
		writeErr(w, 400, "未知更新操作")
		return
	}
	if err := s.startImageUpdate(action, false); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	s.handleImageUpdates(w, r)
}
func (s *Server) imageUpdateLoop() {
	for {
		policy, _, _ := s.cfg.ImageUpdateState()
		if policy.Enabled && !s.imageUpdater().Snapshot().Running {
			if err := s.startImageUpdate("update", true); err != nil {
				log.Printf("image update: %v", err)
			}
		}
		if !waitInterval(s.workContext(), time.Minute) {
			return
		}
	}
}
