package server

import (
	"net/http"

	"agentbox/internal/clientidentity"
	"agentbox/internal/store"
)

func (s *Server) clientIdentity() (string, error) {
	s.clientIdentityOnce.Do(func() {
		s.clientInstanceID, s.clientIdentityErr = clientidentity.LoadOrCreate(s.cfg.DataDir)
	})
	return s.clientInstanceID, s.clientIdentityErr
}

// Optional for the existing transport primitives; binding-aware clients send
// the header on every request. Check before leases, reads or file publication.
func (s *Server) withSyncIdentity(next func(http.ResponseWriter, *http.Request, store.Session)) func(http.ResponseWriter, *http.Request, store.Session) {
	return func(w http.ResponseWriter, r *http.Request, session store.Session) {
		id, err := s.clientIdentity()
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "桌面同步身份不可用，请检查服务端身份文件")
			return
		}
		w.Header().Set("X-Agentbox-Server-ID", id)
		if expected := r.Header.Get("X-Agentbox-Server-ID"); expected != "" && expected != id {
			writeErr(w, http.StatusConflict, "服务器身份已改变，请重新确认同步绑定")
			return
		}
		next(w, r, session)
	}
}
