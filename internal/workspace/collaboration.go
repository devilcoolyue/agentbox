package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/dockerx"
	"agentbox/internal/safefs"

	"agentbox/internal/store"
)

type collaborationStore interface {
	Collaboration(store.User, string) (store.CollaborationRun, error)
	CollaborationParticipants(store.User, string) ([]store.CollaborationParticipant, error)
	ConfirmCollaborationStopped(store.User, string, int64, []store.CollaborationParticipant) error
	CollaborationWork(store.User, string) (store.CollaborationWork, error)
	CollaborationImage(store.User, string) (string, error)
	CollaborationArtifact(store.User, string, string) (store.CollaborationArtifact, error)
}

type collaborationRuntime interface {
	EnsureCollaborationRunning(context.Context, store.Session, config.Account, dockerx.CollaborationProfile) (string, error)
}

// StartCollaboration is reachable only after a persistent step claim. It uses
// normal workspace locks/admission/credential convergence, with an explicit
// dedicated profile rather than the ordinary template and /shared layout.
func (s *Service) StartCollaboration(ctx context.Context, u store.User, work store.CollaborationWork, profile dockerx.CollaborationProfile) (store.Session, error) {
	st, ok := s.store.(collaborationStore)
	if !ok {
		return store.Session{}, ErrManagedSession
	}
	dock, ok := s.dock.(collaborationRuntime)
	if !ok {
		return store.Session{}, ErrManagedSession
	}
	lock := s.lock(work.Session.ID)
	if err := lock.acquire(ctx); err != nil {
		return store.Session{}, err
	}
	defer lock.release()
	current, err := st.CollaborationWork(u, work.Run.ID)
	if err != nil {
		return store.Session{}, err
	}
	if current.Step.ID != work.Step.ID || current.Step.Revision != work.Step.Revision || current.Step.State != "starting" || current.Run.State == store.CollabCancelling || current.Session.ID != work.Session.ID {
		return store.Session{}, store.ErrChatConflict
	}
	if profile.RunID != current.Run.ID || profile.ParticipantID != current.Session.ID || string(profile.Role) != current.Step.Role || profile.Home != s.HomeDir(current.Session) {
		return store.Session{}, ErrManagedSession
	}
	base := filepath.Join(s.cfg.DataDir, "users", u.Name, "collaborations", current.Run.ID)
	expectedProject := filepath.Join(base, "work")
	if profile.Role == collaboration.Coordinator || current.Step.Kind == "handoff_repair" {
		if !collaboration.ValidID(current.Step.SnapshotID) {
			return store.Session{}, ErrManagedSession
		}
		expectedProject = filepath.Join(base, "snapshots", current.Step.SnapshotID, "tree")
	}
	image, err := st.CollaborationImage(u, current.Run.ID)
	if err != nil {
		return store.Session{}, err
	}
	if profile.ReadOnly != (current.Step.Kind == "handoff_repair") || profile.ImageID != image || profile.Workspace != expectedProject || profile.Scratch != filepath.Join(base, "scratch", current.Session.ID) || profile.Inputs != filepath.Join(base, "inputs") {
		return store.Session{}, ErrManagedSession
	}
	acct, err := s.account(current.Session)
	if err != nil {
		return store.Session{}, err
	}
	if acct.Type != current.Binding.AgentType || !acct.AllowsModel(current.Binding.Model) {
		return store.Session{}, ErrCredentials
	}
	if current.Session.ContainerID != "" {
		running, e := s.dock.Running(ctx, current.Session.ContainerID)
		if e != nil {
			return store.Session{}, e
		}
		if running {
			return store.Session{}, errors.New("previous collaboration container still running")
		}
	}
	if err = s.starts.acquire(ctx); err != nil {
		return store.Session{}, err
	}
	defer s.starts.release()
	if err = s.admit(ctx, current.Session); err != nil {
		return store.Session{}, err
	}
	if acct.CredentialsDir != "" {
		if err = s.syncCredentials(ctx, acct, current.Session); err != nil {
			return store.Session{}, err
		}
	}
	data, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		return store.Session{}, err
	}
	defer data.Close()
	rel, err := filepath.Rel(s.cfg.DataDir, profile.Workspace)
	if err != nil || !filepath.IsLocal(rel) {
		return store.Session{}, ErrManagedSession
	}
	if profile.Role == collaboration.Coordinator || current.Step.Kind == "handoff_repair" {
		artifact, e := st.CollaborationArtifact(u, current.Run.ID, current.Step.SnapshotID)
		if e != nil {
			return store.Session{}, e
		}
		snapshots, e := data.Sub(filepath.Join("users", u.Name, "collaborations", current.Run.ID, "snapshots"))
		if e != nil {
			return store.Session{}, e
		}
		_, e = collaboration.VerifySnapshot(ctx, snapshots, artifact.ID, artifact.SHA256, collaboration.SnapshotLimits{MaxBytes: 128 << 20, MaxEntries: 10000})
		snapshots.Close()
		if e != nil {
			return store.Session{}, e
		}
	}
	project, err := data.Sub(rel)
	if err != nil {
		return store.Session{}, err
	}
	defer project.Close()
	if err = agent.ValidateCollaborationProject(project); err != nil {
		return store.Session{}, err
	}
	if err = agent.SeedCollaborationHome(current.Session.Agent, profile.Home, acct.CredentialsDir, current.Binding.Model, dockerx.AgentUID, dockerx.AgentGID); err != nil {
		return store.Session{}, err
	}
	opts, err := agent.ResolveTurnOptions(current.Session.Agent, current.Binding.Model, current.Binding.Effort, current.Binding.EffortControl, s.cfg.ConfiguredReasoning(acct, current.Binding.Model))
	if err != nil {
		return store.Session{}, err
	}
	if err = agent.CheckReasoningInheritance(current.Session.Agent, profile.Home, profile.Workspace, acct.Env, opts); err != nil {
		return store.Session{}, err
	}
	cid, err := dock.EnsureCollaborationRunning(ctx, current.Session, acct, profile)
	if err != nil {
		return store.Session{}, err
	}
	cur, err := s.store.Update(current.Session.ID, func(sess *store.Session) {
		sess.ContainerID = cid
		sess.Status = store.StatusRunning
		sess.StopReason = ""
	})
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return store.Session{}, errors.Join(err, s.dock.Remove(cleanup, cid))
	}
	s.activity.Touch(cur.ID)
	// Account proxy/tunnel setup remains the same authorized runtime hook.
	if s.network != nil {
		if err = s.network(ctx, cur); err != nil {
			return cur, err
		}
	}
	return cur, nil
}

func (s *Service) StopCollaborationParticipant(ctx context.Context, u store.User, run, session string) (store.Session, error) {
	st, ok := s.store.(collaborationStore)
	if !ok {
		return store.Session{}, ErrManagedSession
	}
	participants, err := st.CollaborationParticipants(u, run)
	if err != nil {
		return store.Session{}, err
	}
	found := false
	for _, p := range participants {
		if p.SessionID == session {
			found = true
			break
		}
	}
	if !found {
		return store.Session{}, ErrManagedSession
	}
	lock := s.lock(session)
	if err = lock.acquire(ctx); err != nil {
		return store.Session{}, err
	}
	defer lock.release()
	sess, err := s.stop(ctx, session, "")
	if err != nil {
		return sess, err
	}
	if sess.ContainerID != "" {
		running, e := s.dock.Running(ctx, sess.ContainerID)
		if e != nil {
			return sess, e
		}
		if running {
			return sess, errors.New("collaboration stop unconfirmed")
		}
	}
	if acct, e := s.account(sess); e == nil && acct.CredentialsDir != "" && s.syncCredentials != nil {
		if err = s.syncCredentials(ctx, acct, sess); err != nil {
			return sess, err
		}
	}
	return sess, nil
}

// QuiesceCollaboration stops only this task's participants under the same
// session locks used by ordinary lifecycle operations, verifies actual Docker
// state, and records the observed process epochs while locks remain held.
// A task-state/epoch race makes the store acknowledgement fail, not release a
// new writer. No account authorization is needed to stop revoked credentials.
func (s *Service) QuiesceCollaboration(ctx context.Context, u store.User, run string, revision int64) error {
	st, ok := s.store.(collaborationStore)
	if !ok {
		return errors.New("collaboration store unavailable")
	}
	r, err := st.Collaboration(u, run)
	if err != nil {
		return err
	}
	if r.Revision != revision || (r.State != store.CollabPaused && r.State != store.CollabUncertain && r.State != store.CollabCancelling) {
		return store.ErrChatConflict
	}
	participants, err := st.CollaborationParticipants(u, run)
	if err != nil {
		return err
	}
	sort.Slice(participants, func(i, j int) bool { return participants[i].SessionID < participants[j].SessionID })
	var locked []sessionLock
	defer func() {
		for i := len(locked) - 1; i >= 0; i-- {
			locked[i].release()
		}
	}()
	for _, p := range participants {
		lock := s.lock(p.SessionID)
		if err = lock.acquire(ctx); err != nil {
			return err
		}
		locked = append(locked, lock)
	}
	for i, p := range participants {
		sess, exists := s.store.Get(p.SessionID)
		if !exists || sess.User != u.Name || sess.ContainerID != p.ContainerID {
			return store.ErrChatConflict
		}
		if _, err = s.stop(ctx, sess.ID, ""); err != nil {
			return err
		}
		if sess.ContainerID != "" {
			running, e := s.dock.Running(ctx, sess.ContainerID)
			if e != nil {
				return e
			}
			if running {
				return errors.New("collaboration container did not stop")
			}
		}
		participants[i].ContainerID = sess.ContainerID
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return st.ConfirmCollaborationStopped(u, run, revision, participants)
}

// Helpers share the same admission gate as session starts. The callback must
// not acquire session locks: callers hold those BEFORE this global gate. Keeping the gate
// until cleanup reserves one slot without publishing a fake session; helpers
// are serial and cannot race an ordinary start past a one-container limit.
func (s *Service) WithCollaborationHelper(ctx context.Context, user string, fn func() error) error {
	if err := s.starts.acquire(ctx); err != nil {
		return err
	}
	defer s.starts.release()
	if err := s.admit(ctx, store.Session{User: user}); err != nil {
		return err
	}
	return fn()
}
