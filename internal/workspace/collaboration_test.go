package workspace

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agentbox/internal/store"
)

func TestCollaborationQuiesceMustConfirmStopAndOrdinaryStartIsBlocked(t *testing.T) {
	s, runtime, source := fixture(t)
	st := s.store.(*store.Store)
	u := store.User{Name: source.User, Role: store.RoleUser, CreatedAt: time.Now().UTC()}
	if err := st.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	roles := store.CollaborationRoles{Coordinator: store.CollaborationBinding{AgentType: "codex", AccountID: "acct", Model: "planner"}, Executor: store.CollaborationBinding{AgentType: "codex", AccountID: "acct", Model: "worker"}}
	cfg := store.CollaborationConfig{Roles: roles, MaxSteps: 20, MaxRepairs: 2}
	in := store.CollaborationCreateInput{Scope: "fixture", SourceSessionID: source.ID, Repository: ".", Baseline: strings.Repeat("a", 40), Text: "Plan work", Roles: roles, MaxSteps: 20, MaxRepairs: 2}
	r, _, err := st.CreateCollaboration(u, strings.Repeat("b", 32), in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err = st.BeginCollaborationPreparation(u, r.ID, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	main := store.Session{ID: "main", User: u.Name, Agent: "codex", AccountID: "acct", DefaultModel: "planner", Status: store.StatusStopped, CreatedAt: time.Now()}
	worker := main
	worker.ID = "worker"
	worker.DefaultModel = "worker"
	r, step, err := st.PublishCollaborationParticipants(u, r.ID, r.Revision, main, worker, store.CollaborationStepInput{Text: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(t.Context(), main.ID); !errors.Is(err, ErrManagedSession) {
		t.Fatal("ordinary start reached managed home", err)
	}
	if _, err = s.SwitchAccount(t.Context(), main.ID, "other", nil); !errors.Is(err, ErrManagedSession) {
		t.Fatal("ordinary account switch reached participant", err)
	}
	if _, err = st.ClaimCollaborationStep(u, r.ID, step.ID, step.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Update(main.ID, func(sess *store.Session) { sess.ContainerID = "managed-container"; sess.Status = store.StatusRunning }); err != nil {
		t.Fatal(err)
	}
	if err = st.RecoverCollaborations(); err != nil {
		t.Fatal(err)
	}
	r, err = st.Collaboration(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtime.fail = errors.New("synthetic stop failed")
	if err = s.QuiesceCollaboration(t.Context(), u, r.ID, r.Revision); err == nil {
		t.Fatal("failed stop acknowledged")
	}
	participants, err := st.CollaborationParticipants(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range participants {
		if p.SessionID == main.ID && p.ProcessEpoch == p.StoppedEpoch {
			t.Fatal("failed stop released process fence")
		}
	}
	runtime.fail = nil
	if err = s.QuiesceCollaboration(t.Context(), u, r.ID, r.Revision); err != nil {
		t.Fatal(err)
	}
	participants, err = st.CollaborationParticipants(u, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range participants {
		if p.ProcessEpoch != p.StoppedEpoch {
			t.Fatal("stopped process not acknowledged")
		}
	}
	if runtime.starts != 0 {
		t.Fatal("ordinary start executed managed session")
	}
}
