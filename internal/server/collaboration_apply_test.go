package server

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/store"
)

func TestCollaborationApplicationRecoveryPreservesBothTrees(t *testing.T) {
	for _, phase := range []string{"before-switch", "original-moved", "result-moved"} {
		t.Run(phase, func(t *testing.T) {
			s, sess, h := clientTestServer(t)
			u, _ := s.store.GetUser(sess.User)
			s.cfg.SetCollaborationEnabled(true)
			roles := store.CollaborationRoles{Coordinator: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "test"}, Executor: store.CollaborationBinding{AgentType: "claude", AccountID: "shared", Model: "test"}}
			in := store.CollaborationCreateInput{Scope: s.collaborationScope(u), SourceSessionID: sess.ID, Text: "task", Repository: ".", Baseline: strings.Repeat("a", 40), Roles: roles, MaxSteps: 20}
			run, _, err := s.store.CreateCollaboration(u, strings.Repeat("b", 32), in, store.CollaborationConfig{Roles: roles, MaxSteps: 20})
			if err != nil {
				t.Fatal(err)
			}
			snapshot := strings.Repeat("c", 32)
			db, err := sql.Open("sqlite", filepath.Join(s.cfg.DataDir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec("UPDATE collaborations SET state='completed',snapshot_id=? WHERE id=?", snapshot, run.ID); err != nil {
				t.Fatal(err)
			}
			action := strings.Repeat("d", 32)
			if _, _, err = s.store.BeginCollaborationApplication(u, run.ID, action, snapshot, run.Revision); err != nil {
				t.Fatal(err)
			}
			source := s.workspaceDir(sess)
			parent := filepath.Dir(source)
			if err = os.MkdirAll(source, 0755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(source, "original"), []byte("preserved"), 0600)
			if phase != "before-switch" {
				if err = s.store.SetCollaborationApplication(u, run.ID, action, "staging", "switching"); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(source, filepath.Join(parent, "collaboration-before-"+action)); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "result-moved" {
				os.Mkdir(source, 0755)
				os.WriteFile(filepath.Join(source, "result"), []byte("review me"), 0600)
			}
			// Restart fences writes without guessing which rename reached disk.
			if err = s.store.RecoverCollaborations(); err != nil {
				t.Fatal(err)
			}
			blocked := clientRequest(h, u.Name, "PUT", "/api/sessions/"+sess.ID+"/file?path=original", "overwrite")
			if blocked.Code != 409 {
				t.Fatal("write escaped recovery fence", blocked.Code)
			}
			input, _ := json.Marshal(map[string]string{"scope": in.Scope, "action_id": action})
			url := "/api/collaboration/runs/" + run.ID + "/application/recover"
			response := clientRequest(h, u.Name, "POST", url, string(input))
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			b, err := os.ReadFile(filepath.Join(source, "original"))
			if err != nil || string(b) != "preserved" {
				t.Fatal("original lost", string(b), err)
			}
			if phase == "result-moved" {
				b, err = os.ReadFile(filepath.Join(parent, "collaboration-review-"+action, "result"))
				if err != nil || string(b) != "review me" {
					t.Fatal("new copy discarded", err)
				}
			}
			response = clientRequest(h, u.Name, "POST", url, string(input))
			if response.Code != 200 {
				t.Fatal("recovery replay", response.Body.String())
			}
			if blocked, err := s.store.CollaborationApplicationBlocked(sess.ID); blocked || err != nil {
				t.Fatal(blocked, err)
			}
		})
	}
}
