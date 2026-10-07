package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"agentbox/internal/config"
)

func TestSessionModelsOwnershipAccessAndOverrides(t *testing.T) {
	s, sess := accessTestServer(t)
	sess.AccountID, sess.DefaultModel = "shared", "claude-opus-5"
	s.store.Put(sess)
	policies := map[string]config.ReasoningCapability{sess.DefaultModel: {Support: "supported", Control: "effort", Levels: []string{"low", "high"}}, "custom": {Support: "unsupported"}}
	if _, err := s.cfg.UpdateAccount("shared", config.AccountPatch{ModelReasoning: &policies}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		user string
		code int
	}{{"alice", 200}, {"bob", 404}} {
		w := httptest.NewRecorder()
		r := accessRequest(tc.user, "GET", "/api/sessions/s1/models", "")
		r.SetPathValue("id", sess.ID)
		s.withSession(s.handleSessionModels).ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.user, w.Code, w.Body.String())
		}
		if tc.code != 200 {
			continue
		}
		var view sessionModelsView
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, model := range view.Models {
			if model.ID == sess.DefaultModel {
				found = model.Reasoning.Control == "effort" && len(model.Reasoning.Levels) == 2
			}
		}
		if !found {
			t.Fatal("account override missing")
		}
		for _, private := range []string{"credentials", "base_url", "SECRET", "access", "proxy"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("leaked %s", private)
			}
		}
	}
	deny := &config.AccountAccess{Mode: "admin"}
	if _, err := s.cfg.UpdateAccount("shared", config.AccountPatch{Access: deny}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleSessionModels(w, accessRequest("alice", "GET", "/models", ""), sess)
	if w.Code != 403 {
		t.Fatalf("withdrawal ignored: %d", w.Code)
	}
}

func TestInvalidEffortRejectedBeforeDockerAndUserLog(t *testing.T) {
	s, sess := accessTestServer(t)
	sess.AccountID, sess.DefaultModel = "shared", "claude-opus-5"
	s.store.Put(sess)
	policies := map[string]config.ReasoningCapability{sess.DefaultModel: {Support: "unsupported"}}
	if _, err := s.cfg.UpdateAccount("shared", config.AccountPatch{ModelReasoning: &policies}); err != nil {
		t.Fatal(err)
	}
	room := s.chat.room(sess.ID)
	room.tryBegin()
	// Docker is nil. This must return an error before startup or inference.
	room.runTurn("must not run", sess.DefaultModel, "high", "effort")
	tid := s.activeThread(sess)
	raw, err := os.ReadFile(s.threadPath(sess, tid))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"kind":"user"`) || !strings.Contains(string(raw), `"code":"chat_options_invalid"`) {
		t.Fatalf("invalid request was logged as a user turn: %s", raw)
	}
	if room.state() != "idle" {
		t.Fatal("rejected request left room running")
	}
}
