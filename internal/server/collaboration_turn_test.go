package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/chat"
	"agentbox/internal/collaboration"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

type preparedCollaborationChat struct{ collaborationChatRuntime }

func (r preparedCollaborationChat) Start(context.Context, store.Session) (store.Session, error) {
	return r.turn.work.Session, nil
}

// Exercises the common execution/settlement path using the existing synthetic
// Docker exec fixture. Container preparation and app-server capabilities have
// separate tests; this test does not claim a real provider or full scheduler.
func TestCollaborationTurnUsesOriginalUsageAndHistoryFinalization(t *testing.T) {
	for _, scenario := range []string{"success", "usage-failure", "history-failure"} {
		t.Run(scenario, func(t *testing.T) {
			payload := `{"version":1,"type":"plan","payload":{"summary":"Plan","items":[{"id":"one","text":"Implement","acceptance":["test"]}]}}`
			message, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "phase": "final_answer", "text": payload}})
			f := newReceiptFixture(t, `{"type":"thread.started","thread_id":"fixture-thread"}`+"\n"+string(message)+"\n"+receiptCompletedOutput, false)
			roles := store.CollaborationRoles{Coordinator: store.CollaborationBinding{AgentType: "codex", AccountID: "shared", Model: "gpt-5.5"}, Executor: store.CollaborationBinding{AgentType: "codex", AccountID: "shared", Model: "gpt-5.5"}}
			input := store.CollaborationCreateInput{Scope: "synthetic", SourceSessionID: f.sess.ID, Text: "task", Repository: ".", Baseline: strings.Repeat("a", 40), Roles: roles, MaxSteps: 20, MaxRepairs: 2}
			cfg := store.CollaborationConfig{Roles: roles, MaxSteps: 20, MaxRepairs: 2}
			r, _, err := f.s.store.CreateCollaboration(f.u, strings.Repeat("b", 32), input, cfg)
			if err != nil {
				t.Fatal(err)
			}
			r, err = f.s.store.BeginCollaborationPreparation(f.u, r.ID, r.Revision)
			if err != nil {
				t.Fatal(err)
			}
			main := store.Session{ID: store.NewCollaborationID(), User: f.u.Name, Agent: "codex", AccountID: "shared", DefaultModel: "gpt-5.5", Status: store.StatusStopped, CreatedAt: time.Now()}
			worker := main
			worker.ID = store.NewCollaborationID()
			r, step, err := f.s.store.PublishCollaborationParticipants(f.u, r.ID, r.Revision, main, worker, store.CollaborationStepInput{Text: f.input.Text})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.store.Update(main.ID, func(s *store.Session) { s.ContainerID = "fixture"; s.Status = store.StatusRunning }); err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.store.ClaimCollaborationStep(f.u, r.ID, step.ID, step.Revision); err != nil {
				t.Fatal(err)
			}
			work, err := f.s.store.CollaborationWork(f.u, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{f.s.homeDir(work.Session), filepath.Join(f.s.sessionDir(work.Session), "chats"), f.s.workspaceDir(work.Session)} {
				if err = os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = f.s.store.Grant(f.u.Name, 1000, "fixture", "", "admin"); err != nil {
				t.Fatal(err)
			}
			if scenario == "usage-failure" {
				db, e := sql.Open("sqlite", filepath.Join(f.s.cfg.DataDir, "state.db"))
				if e != nil {
					t.Fatal(e)
				}
				defer db.Close()
				if _, e = db.Exec(`CREATE TRIGGER collab_usage_failure BEFORE INSERT ON usage_events BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "history-failure" {
				if err = os.Mkdir(filepath.Join(f.s.sessionDir(work.Session), "chats", r.ID+".jsonl"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			turn := &collaborationTurn{srv: f.s, owner: f.u, work: work, profile: dockerx.CollaborationProfile{Home: f.s.homeDir(work.Session), Workspace: f.s.workspaceDir(work.Session)}, final: agent.NewFinalOutput("codex", "", collaboration.MaxHandoffBytes)}
			f.unblock()
			service := chat.Service{Runtime: preparedCollaborationChat{collaborationChatRuntime{turn}}, Usage: f.s.usageService()}
			service.Run(t.Context(), chat.Input{SessionID: main.ID, Text: work.Input.Text, Model: "gpt-5.5", RequireUsage: true}, turn)
			if turn.Succeeded() != (scenario == "success") {
				t.Fatal("incorrect completion", turn.code, turn.failed, turn.historyFailed, turn.usageFailed)
			}
			if scenario == "success" {
				answer, err := turn.final.Result()
				if err != nil || answer != payload {
					t.Fatal(answer, err)
				}
				rows := f.s.store.ListUsage(store.UsageFilter{User: f.u.Name})
				q, _ := f.s.store.GetQuota(f.u.Name)
				if len(rows) != 1 || rows[0].TurnID != step.TurnID || rows[0].ThreadID != r.ID || rows[0].SessionID != main.ID || q.BalanceMicroUSD != 850 || f.invocations.Load() != 1 {
					t.Fatal("lost or duplicate accounting", rows, q)
				}
				view := getUsageEvents(t, f.s, "/api/usage/events?kind=collaboration&session="+f.sess.ID, f.u.Name, store.RoleUser)
				if len(view.Rows) != 1 || view.Rows[0].Kind != "collaboration" || view.Rows[0].Collaboration == nil || view.Rows[0].Collaboration.StepID != step.ID || view.Total.CostMicroUSD != 150 {
					t.Fatal("settled collaboration turn missing from source workspace usage", view)
				}
			} else if scenario == "history-failure" && f.invocations.Load() != 0 {
				t.Fatal("model started before durable input history")
			}
		})
	}
}
