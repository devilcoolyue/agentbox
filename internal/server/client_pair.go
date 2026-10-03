package server

import (
	"net/http"
	"strings"
)

func (s *Server) clientPairStore() *pairStore {
	s.clientPairsOnce.Do(func() { s.clientPairs = newPairStore() })
	return s.clientPairs
}

// Codes have a distinct store and endpoint from abox-link. Neither enabling nor
// disabling the tunnel changes desktop pairing, and codes cannot cross channels.
func (s *Server) handleClientPair(w http.ResponseWriter, r *http.Request) {
	code, ok := s.clientPairStore().issue(reqUser(r).Name)
	if !ok {
		writeErr(w, http.StatusTooManyRequests, "待用配对码过多，请稍后重试")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires_in": int(pairCodeTTL.Seconds())})
}

func (s *Server) handleClientPairRedeem(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.logins.blocked(ip) {
		writeErr(w, http.StatusTooManyRequests, "配对尝试过于频繁，请稍后重试")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := decodeClientRequest(w, r, &body); err != nil {
		writeClientError(w, err)
		return
	}
	user, ok := s.clientPairStore().redeem(strings.TrimSpace(body.Code))
	u, exists := s.store.GetUser(user)
	if !ok || !exists {
		s.logins.fail(ip)
		writeErr(w, http.StatusUnauthorized, "配对码无效或已过期，请重新生成")
		return
	}
	token := newToken()
	if err := s.store.CreateToken(token, user); err != nil {
		writeErr(w, http.StatusInternalServerError, "创建登录失败，请重新配对")
		return
	}
	s.logins.success(ip)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "user": user, "role": u.Role})
}
