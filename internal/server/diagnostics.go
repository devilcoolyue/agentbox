package server

import (
	"agentbox/internal/buildinfo"
	"agentbox/internal/config"
	"agentbox/internal/diagnostics"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"context"
	"log"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gorilla/websocket"
)

// Allowlisted data only: never include paths, identities, config, environment,
// logs, proxy URLs, raw Docker inspect or transcripts in a shareable report.
func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	report := diagnostics.Instance(ctx, s.cfg, s.diagnosticRuntime(), false)
	s.correlateDiagnostics(r, &report)
	total, free := readDiskUsage(s.cfg.DataDir)
	writeJSON(w, http.StatusOK, map[string]any{
		"version": buildinfo.Version, "revision": buildinfo.Commit(), "built_at": buildinfo.BuiltAt,
		"go_version": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		"schema_version": store.SchemaVersion, "started_at": s.startedAt.UnixMilli(),
		"sessions": len(s.store.All()), "disk_total": total, "disk_available": free,
		"resources": s.cfg.GetResources(), "usage_sync": s.usageService().SyncStatus(),
		"environment": report,
	})
}

func (s *Server) diagnosticRuntime() diagnostics.Runtime {
	if s.dock == nil {
		return nil
	}
	return s.dock
}

func (s *Server) correlateDiagnostics(r *http.Request, report *diagnostics.Report) {
	id, _ := r.Context().Value(operationContextKey{}).(string)
	if id == "" {
		id = newOperationID()
	}
	report.OperationID = id
	failed, unchecked := 0, 0
	for _, check := range report.Checks {
		if check.State == diagnostics.Failed {
			failed++
			log.Printf("diagnostic_check operation_id=%s scope=%s check=%s state=%s code=%s", id, report.Scope, check.ID, check.State, check.Code)
		}
		if check.State == diagnostics.NotChecked {
			unchecked++
		}
	}
	log.Printf("diagnostics_completed operation_id=%s scope=%s failed=%d unchecked=%d", id, report.Scope, failed, unchecked)
}

func (s *Server) handleCheckDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	report := diagnostics.Instance(ctx, s.cfg, s.diagnosticRuntime(), true)
	s.correlateDiagnostics(r, &report)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleSessionDiagnostics(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	report := diagnostics.NewReport("session")
	// Hold the normal workspace lock so purge cannot remove/recreate paths
	// while they are being inspected. This path never starts a container.
	err := s.workspaces().WithSession(ctx, sess.ID, func(current store.Session) error {
		if _, err := s.sessionAccount(current); err != nil {
			report.Checks = append(report.Checks, diagnostics.Result("account_access", diagnostics.Failed, "account_access_denied"))
			for _, id := range []string{"docker", "agent_image", "data_disk", "workspace_permissions", "account_configuration"} {
				report.Checks = append(report.Checks, diagnostics.Result(id, diagnostics.NotChecked, "dependency_unavailable"))
			}
		} else {
			report.Checks = append(report.Checks, diagnostics.Result("account_access", diagnostics.Passed, "account_access_ok"))
			report.Checks = append(report.Checks, diagnostics.RuntimeChecks(ctx, s.diagnosticRuntime(), s.cfg.GetAgentImage())...)
			report.Checks = append(report.Checks, diagnostics.Disk(s.cfg.DataDir, s.cfg.GetResources().MinFreeBytes))
			root, err := safefs.Open(s.cfg.DataDir)
			if err != nil {
				report.Checks = append(report.Checks, diagnostics.Result("workspace_permissions", diagnostics.Failed, "storage_unavailable"))
			} else {
				base := filepath.Join("users", current.User, "sessions", current.ID)
				report.Checks = append(report.Checks, diagnostics.WorkspacePermissions(root, filepath.Join(base, "workspace"), filepath.Join(base, "home")))
				root.Close()
			}
			// Recheck authorization after the potentially slow Docker probes.
			acct, err := s.sessionAccount(current)
			if err != nil {
				report.Checks = append(report.Checks, diagnostics.Result("account_configuration", diagnostics.NotChecked, "dependency_unavailable"))
				report.Checks[0] = diagnostics.Result("account_access", diagnostics.Failed, "account_access_denied")
			} else {
				report.Checks = append(report.Checks, diagnostics.Accounts([]config.Account{acct}))
			}
		}
		quota := diagnostics.Result("quota", diagnostics.Passed, "quota_ok")
		if s.quotaBlock(current.User) != "" {
			quota = diagnostics.Result("quota", diagnostics.Failed, "quota_exhausted")
		}
		report.Checks = append(report.Checks, quota)
		return nil
	})
	if err != nil {
		writeProblem(w, r, "session.diagnostics", classifyProblem(err, "internal_error"))
		return
	}
	report = diagnostics.Finish(report)
	s.correlateDiagnostics(r, &report)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, report)
}

// Dedicated WS probe: authenticate and enforce same-origin normally, send one
// fixed observation, then close. No container, chat room or provider is touched.
func (s *Server) handleDiagnosticWS(w http.ResponseWriter, r *http.Request) {
	upgrader := s.upgrader
	upgrader.Error = func(w http.ResponseWriter, r *http.Request, status int, _ error) {
		p := operationProblem(r.Context(), "diagnostics.websocket", "websocket_failed")
		w.Header().Set("X-Agentbox-Operation-ID", p.OperationID)
		writeJSON(w, status, p)
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	release, ok := s.track(func() { _ = conn.Close() })
	if !ok {
		return
	}
	defer release()
	defer conn.Close()
	id, _ := r.Context().Value(operationContextKey{}).(string)
	if id == "" {
		id = newOperationID()
	}
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := conn.WriteJSON(struct {
		Type        string `json:"type"`
		OperationID string `json:"operation_id"`
		diagnostics.Check
	}{"diagnostic", id, diagnostics.Result("websocket", diagnostics.Passed, "websocket_ok")}); err != nil {
		operationProblem(r.Context(), "diagnostics.websocket", "websocket_failed")
		return
	}
	log.Printf("diagnostic_websocket_response operation_id=%s sent=true", id)
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
}
