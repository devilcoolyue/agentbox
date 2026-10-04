package server

import (
	"errors"
	"net/http"

	"agentbox/internal/store"
	"agentbox/internal/syncproto"
)

func (s *Server) syncLeases() *syncproto.Leases {
	s.clientLeasesOnce.Do(func() { s.clientLeases = syncproto.NewLeases() })
	return s.clientLeases
}

// The lease coordinates sync clients only. It cannot lock IDE/CLI writes;
// conditional writes and recovery copies must still check every target.
func (s *Server) handleClientLease(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var request struct {
		Project    string `json:"project"`
		Device     string `json:"device"`
		Action     string `json:"action"`
		Generation string `json:"generation"`
	}
	if err := decodeClientRequest(w, r, &request); err != nil {
		writeClientError(w, err)
		return
	}
	var lease syncproto.Lease
	err := s.workspaces().WithSession(r.Context(), sess.ID, func(current store.Session) error {
		project, err := s.store.ClientProject(current.ID, request.Project)
		if err != nil {
			return err
		}
		switch request.Action {
		case "acquire":
			if err = s.checkClientProjectDir(current, project.Path); err != nil {
				return err
			}
			lease, err = s.syncLeases().Acquire(current.ID, project.ID, project.Path, request.Device)
		case "renew":
			lease, err = s.syncLeases().Renew(current.ID, project.ID, project.Path, request.Device, r.Header.Get("X-Agentbox-Sync-Lease"), request.Generation)
		case "release":
			err = s.syncLeases().Release(current.ID, project.ID, project.Path, request.Device, r.Header.Get("X-Agentbox-Sync-Lease"), request.Generation)
		default:
			return store.ErrClientInvalid
		}
		return err
	})
	if errors.Is(err, syncproto.ErrLeaseBusy) || errors.Is(err, syncproto.ErrLeaseExpired) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, syncproto.ErrLimit) {
		writeErr(w, http.StatusTooManyRequests, "同步租约已达上限")
		return
	}
	if errors.Is(err, syncproto.ErrInvalid) {
		writeClientError(w, store.ErrClientInvalid)
		return
	}
	if err != nil {
		writeClientError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if request.Action == "release" {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, lease)
}
