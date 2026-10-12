package server

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/archivex"
	"agentbox/internal/collaboration"
	"agentbox/internal/dockerx"
	"agentbox/internal/gitx"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

var collaborationSnapshotLimits = collaboration.SnapshotLimits{MaxBytes: 128 << 20, MaxEntries: 10000}

func (s *Server) collaborationDir(u store.User, run string) string {
	return filepath.Join(s.cfg.DataDir, "users", u.Name, "collaborations", run)
}

func (s *Server) validateCollaborationBindings(u store.User, cfg store.CollaborationConfig) error {
	current, ok := s.store.GetUser(u.Name)
	if !ok || !current.CreatedAt.Equal(u.CreatedAt) {
		return errAccountAccess
	}
	if err := cfg.Roles.Validate(); err != nil {
		return err
	}
	if cfg.Workflow != nil {
		if err := cfg.Workflow.Validate(); err != nil {
			return err
		}
	}
	for _, role := range cfg.ActiveRoles() {
		b, _ := cfg.Roles.Binding(role)
		a, ok := s.cfg.Account(b.AccountID)
		if !ok || a.Type != b.AgentType || !a.CanUse(u.Name, current.Role == store.RoleAdmin) || !a.AllowsModel(b.Model) {
			return errAccountAccess
		}
		if _, err := agent.ResolveTurnOptions(b.AgentType, b.Model, b.Effort, b.EffortControl, s.cfg.ConfiguredReasoning(a, b.Model)); err != nil {
			return err
		}
	}
	return nil
}

// prepareCollaboration runs only for the owner of a durable preparation claim.
// Source Git is offline/read-only; there is no ordinary workspace start or
// source credential seeding. Intermediate output stays in task-owned staging.
func (s *Server) prepareCollaboration(ctx context.Context, u store.User, r store.CollaborationRun, image string) (store.CollaborationRun, error) {
	if r.State != store.CollabPreparing || r.Request == nil {
		return r, store.ErrChatConflict
	}
	cfg, err := s.store.CollaborationConfig(r.ID, r.ConfigVersion)
	if err != nil {
		return r, err
	}
	if err = s.validateCollaborationBindings(u, cfg); err != nil {
		return r, err
	}
	data, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		return r, err
	}
	defer data.Close()
	rel := filepath.Join("users", u.Name, "collaborations", r.ID)
	for _, dir := range []string{rel, filepath.Join(rel, "staging"), filepath.Join(rel, "snapshots"), filepath.Join(rel, "inputs"), filepath.Join(rel, "scratch")} {
		if err = data.MkdirAll(dir, 0700); err != nil {
			return r, err
		}
	}
	root, err := data.Sub(rel)
	if err != nil {
		return r, err
	}
	defer root.Close()
	if err = root.ChmodDir("inputs", 0755); err != nil {
		return r, err
	}
	var snapshot collaboration.Snapshot
	_, workErr := root.Lstat("work")
	if workErr != nil && !os.IsNotExist(workErr) {
		return r, workErr
	}
	if os.IsNotExist(workErr) {
		if err = root.Mkdir("staging/import", 0700); err != nil {
			return r, err
		}
		stage, err := root.Sub("staging/import")
		if err != nil {
			return r, err
		}
		if err = stage.Mkdir("tree", 0755); err != nil {
			stage.Close()
			return r, err
		}
		tree, err := stage.Sub("tree")
		if err != nil {
			stage.Close()
			return r, err
		}
		err = s.workspaces().WithSession(ctx, r.SourceSessionID, func(source store.Session) error {
			if source.User != u.Name {
				return errAccountAccess
			}
			if _, e := s.sessionAccount(source); e != nil {
				return e
			}
			return s.workspaces().WithCollaborationHelper(ctx, u.Name, func() error {
				return s.dock.WithProjectReader(ctx, image, s.workspaceDir(source), func(cid string) error {
					git := gitx.New(s.dock, func(context.Context, string) (string, func(), error) { return cid, func() {}, nil })
					check := func() error {
						head, e := git.Run(ctx, source.ID, r.Request.Repository, "rev-parse", "--verify", "HEAD")
						if e != nil {
							return e
						}
						if strings.TrimSpace(head) != r.Request.Baseline {
							return errors.New("project baseline changed")
						}
						status, e := git.Run(ctx, source.ID, r.Request.Repository, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
						if e != nil {
							return e
						}
						if status != "" {
							return errors.New("project has uncommitted changes")
						}
						return nil
					}
					if e := check(); e != nil {
						return e
					}
					listing, e := git.Run(ctx, source.ID, r.Request.Repository, "ls-tree", "-rlz", "--full-tree", r.Request.Baseline)
					if e != nil {
						return e
					}
					entries, e := collaboration.ParseGitTree([]byte(listing), collaborationSnapshotLimits)
					if e != nil {
						return e
					}
					reader, writer := io.Pipe()
					done := make(chan error, 1)
					go func() {
						e := git.Archive(ctx, source.ID, r.Request.Repository, r.Request.Baseline, writer, collaborationSnapshotLimits.MaxBytes+32<<20)
						_ = writer.CloseWithError(e)
						done <- e
					}()
					importErr := collaboration.ImportGitArchive(ctx, reader, tree, entries, collaborationSnapshotLimits)
					_ = reader.CloseWithError(importErr)
					if e = errors.Join(importErr, <-done); e != nil {
						return e
					}
					return check()
				})
			})
		})
		if err == nil {
			err = agent.ValidateCollaborationProject(tree)
		}
		if err == nil {
			snapshots, e := root.Sub("snapshots")
			if e != nil {
				err = e
			} else {
				snapshot, err = collaboration.PublishSnapshot(ctx, tree, snapshots, store.NewCollaborationID(), collaborationSnapshotLimits)
				snapshots.Close()
			}
		}
		if err == nil {
			err = archivex.ChownRoot(tree, dockerx.AgentUID, dockerx.AgentGID)
		}
		tree.Close()
		stage.Close()
		if err != nil {
			return r, errors.Join(err, root.RemoveAll("staging/import"))
		}
		if err = root.RenameTo("staging/import/tree", root, "work", false); err != nil {
			return r, err
		}
		if err = root.RemoveAll("staging/import"); err != nil {
			return r, err
		}
		if err = root.Sync(); err != nil {
			return r, err
		}
		if err = s.workspaces().WithCollaborationHelper(ctx, u.Name, func() error {
			return s.dock.InitializeCollaborationProject(ctx, image, filepath.Join(s.collaborationDir(u, r.ID), "work"))
		}); err != nil {
			return r, err
		}
		if _, err = s.store.RegisterCollaborationSnapshot(u, r.ID, r.Revision, "", 0, snapshot); err != nil {
			return r, err
		}
	} else {
		// Configuration changes reuse the existing work; they never re-import
		// the old source commit and discard the executor's partial result.
		work, e := root.Sub("work")
		if e != nil {
			return r, e
		}
		defer work.Close()
		if err = agent.ValidateCollaborationProject(work); err != nil {
			return r, err
		}
		snapshots, e := root.Sub("snapshots")
		if e != nil {
			return r, e
		}
		defer snapshots.Close()
		snapshot, err = collaboration.PublishSnapshot(ctx, work, snapshots, store.NewCollaborationID(), collaborationSnapshotLimits)
		if err != nil {
			return r, err
		}
		if _, err = s.store.RegisterCollaborationSnapshot(u, r.ID, r.Revision, "", 0, snapshot); err != nil {
			return r, err
		}
	}
	makeSession := func(b store.CollaborationBinding, role string) (store.Session, error) {
		sess := store.Session{ID: store.NewCollaborationID(), User: u.Name, Name: "collaboration " + role, Agent: b.AgentType, AccountID: b.AccountID, DefaultModel: b.Model, Status: store.StatusStopped, CreatedAt: time.Now().UTC()}
		base := filepath.Join("users", u.Name, "sessions", sess.ID)
		for _, d := range []string{base, filepath.Join(base, "home"), filepath.Join(base, "chats"), filepath.Join(rel, "scratch", sess.ID)} {
			if e := data.MkdirAll(d, 0700); e != nil {
				return sess, e
			}
		}
		for _, d := range []string{filepath.Join(base, "home"), filepath.Join(rel, "scratch", sess.ID)} {
			if e := data.Chown(d, dockerx.AgentUID, dockerx.AgentGID); e != nil {
				return sess, e
			}
		}
		return sess, nil
	}
	var main store.Session
	if !cfg.Workflow.IsDirect() {
		main, err = makeSession(cfg.Roles.Coordinator, "coordinator")
		if err != nil {
			return r, err
		}
	}
	worker, err := makeSession(cfg.Roles.Executor, "executor")
	if err != nil {
		return r, err
	}
	// Versioned workflows freeze their initial context inside the same store
	// transaction that publishes participants and the first execution step.
	// Legacy tasks keep their existing prompt and full-history behavior.
	var prompt string
	if cfg.Workflow == nil {
		messages, e := s.store.CollaborationMessages(u, r.ID, 0)
		if e != nil {
			return r, e
		}
		contextMessages := []collaboration.ContextMessage{}
		for _, m := range messages {
			contextMessages = append(contextMessages, collaboration.ContextMessage{ID: m.ID, Sender: m.SenderRole, Type: m.Type, Body: m.Body})
		}
		prompt, err = collaboration.BuildPrompt(collaboration.Coordinator, "plan", r.Request.PromptGoal(), snapshot.ID, r.PlanVersion, contextMessages)
		if err != nil {
			return r, err
		}
	}
	current, err := s.store.Collaboration(u, r.ID)
	if err != nil {
		return r, err
	}
	if current.ConfigVersion != r.ConfigVersion {
		return r, store.ErrChatConflict
	}
	if current.State == store.CollabCancelling {
		return current, nil
	}
	updated, _, err := s.store.PublishCollaborationParticipants(u, r.ID, current.Revision, main, worker, store.CollaborationStepInput{Text: prompt, SnapshotID: snapshot.ID})
	if err != nil {
		return r, err
	}
	return updated, nil
}

func (s *Server) collaborationProfile(u store.User, w store.CollaborationWork, image string) (dockerx.CollaborationProfile, error) {
	base := s.collaborationDir(u, w.Run.ID)
	p := dockerx.CollaborationProfile{ReadOnly: w.Step.Kind == "handoff_repair", RunID: w.Run.ID, ParticipantID: w.Session.ID, Role: collaboration.Role(w.Step.Role), ImageID: image, Home: s.homeDir(w.Session), Scratch: filepath.Join(base, "scratch", w.Session.ID), Inputs: filepath.Join(base, "inputs"), Workspace: filepath.Join(base, "work")}
	if p.Role == collaboration.Coordinator || p.ReadOnly {
		a, err := s.store.CollaborationArtifact(u, w.Run.ID, w.Step.SnapshotID)
		if err != nil {
			return p, err
		}
		p.Workspace = filepath.Join(base, a.StorageKey, "tree")
	}
	return p, nil
}
