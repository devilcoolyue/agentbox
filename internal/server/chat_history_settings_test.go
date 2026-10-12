package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentbox/internal/store"
)

func TestChatHistoryTimeoutSettingsAPI(t *testing.T) {
	s := pricingTestServer(t)
	s.tunnels = newTunnelHub()
	response := pricingRequest(s.handlePutSettings, http.MethodPut, map[string]int64{"chat_history_timeout_sec": 240, "max_upload_mb": 128})
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	var saved settingsView
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil || saved.ChatHistoryTimeoutSec != 240 || saved.MaxUploadMB != 128 {
		t.Fatalf("settings response: %+v, %v", saved, err)
	}
	get := httptest.NewRecorder()
	s.handleGetSettings(get, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if err := json.Unmarshal(get.Body.Bytes(), &saved); err != nil || saved.ChatHistoryTimeoutSec != 240 {
		t.Fatal("GET settings lost timeout", err)
	}
	for _, role := range []string{store.RoleUser, store.RoleAdmin} {
		r := httptest.NewRequest(http.MethodGet, "/api/me", nil).WithContext(context.WithValue(t.Context(), ctxUser, store.User{Name: "alice", Role: role}))
		w := httptest.NewRecorder()
		s.handleMe(w, r)
		var me struct {
			Timeout int64 `json:"chat_history_timeout_sec"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil || w.Code != http.StatusOK || me.Timeout != 240 {
			t.Fatalf("timeout not delivered to %s: %s (%v)", role, w.Body, err)
		}
	}
	response = pricingRequest(s.handlePutSettings, http.MethodPut, map[string]int64{"chat_history_timeout_sec": 0, "max_upload_mb": 64})
	if response.Code != http.StatusBadRequest || s.cfg.GetChatHistoryTimeoutSec() != 240 || s.cfg.GetMaxUploadMB() != 128 {
		t.Fatal("invalid timeout was accepted or changed upload limit", response.Code)
	}
}
