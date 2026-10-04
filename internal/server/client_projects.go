package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"agentbox/internal/store"
	"agentbox/internal/workspace"
)

func (s *Server) handleClientCapabilities(w http.ResponseWriter, r *http.Request) {
	id, err := s.clientIdentity()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "桌面同步身份不可用，请检查服务端身份文件")
		return
	}
	syncVersion := 0
	if s.cfg.GetDesktopSyncEnabled() {
		syncVersion = 1
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"protocol_version": 1,
		"server_id":        id,
		"user":             reqUser(r).Name,
		"features":         map[string]int{"pairing": 1, "project_terminals": 1, "sync": syncVersion, "sync_recovery_gc": 1, "sync_recovery_inspect": 1},
		"limits":           map[string]int{"projects_per_workspace": store.ClientProjectLimit, "terminals_per_workspace": store.ClientTerminalLimit},
	})
}

func decodeClientRequest(w http.ResponseWriter, r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return store.ErrClientInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return store.ErrClientInvalid
	}
	return nil
}

func writeClientError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrClientInvalid):
		writeErr(w, http.StatusBadRequest, "项目名称、相对目录或启动参数无效")
	case errors.Is(err, store.ErrClientMissing), errors.Is(err, workspace.ErrSessionGone):
		writeErr(w, http.StatusNotFound, "空间、项目或终端不存在")
	case errors.Is(err, store.ErrClientConflict):
		writeErr(w, http.StatusConflict, "项目已更改、目录已映射或仍有终端；请刷新后重试")
	case errors.Is(err, store.ErrClientLimit):
		writeErr(w, http.StatusConflict, "已达到此空间的项目或终端数量上限")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusRequestTimeout, "操作已取消或超时")
	default:
		writeErr(w, http.StatusInternalServerError, "项目或终端操作失败，请重试")
	}
}

// Resolving through pinned handles prevents a project mapping from following a
// container-created symlink into another host directory. No files are moved.
func (s *Server) checkClientProjectDir(sess store.Session, relative string) error {
	if !store.ClientProjectPath(relative) {
		return store.ErrClientInvalid
	}
	root, err := s.openDataDir(s.workspaceDir(sess))
	if err != nil {
		return store.ErrClientInvalid
	}
	defer root.Close()
	dir, err := root.Sub(relative)
	if err != nil {
		return store.ErrClientInvalid
	}
	return dir.Close()
}

func (s *Server) handleClientProjects(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if r.Method == http.MethodGet {
		projects, err := s.store.ClientProjects(sess.ID)
		if err != nil {
			writeClientError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, projects)
		return
	}
	var body struct {
		Name      string   `json:"name"`
		Path      string   `json:"path"`
		Arguments []string `json:"arguments"`
		Revision  int64    `json:"revision"`
	}
	if err := decodeClientRequest(w, r, &body); err != nil {
		writeClientError(w, err)
		return
	}
	project := store.ClientProject{ID: r.PathValue("project"), SessionID: sess.ID, Name: body.Name, Path: body.Path, Arguments: body.Arguments, Revision: body.Revision}
	err := s.workspaces().WithSession(r.Context(), sess.ID, func(current store.Session) error {
		if r.Method != http.MethodPost && s.syncLeases().Active(current.ID, project.ID) {
			return store.ErrClientConflict
		}
		if err := s.checkClientProjectDir(current, project.Path); err != nil {
			return err
		}
		var err error
		if r.Method == http.MethodPost {
			project, err = s.store.CreateClientProject(project)
		} else {
			project, err = s.store.UpdateClientProject(project)
		}
		return err
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, project)
}

func (s *Server) handleClientProjectDelete(w http.ResponseWriter, r *http.Request, sess store.Session) {
	revision, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || revision < 1 {
		writeClientError(w, store.ErrClientInvalid)
		return
	}
	err = s.workspaces().WithSession(r.Context(), sess.ID, func(store.Session) error {
		if s.syncLeases().Active(sess.ID, r.PathValue("project")) {
			return store.ErrClientConflict
		}
		return s.store.DeleteClientProject(sess.ID, r.PathValue("project"), revision)
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleClientTerminals(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if r.Method == http.MethodGet {
		terminals, err := s.store.ClientTerminals(sess.ID)
		if err != nil {
			writeClientError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, terminals)
		return
	}
	if why := s.quotaBlock(sess.User); why != "" {
		writeErr(w, http.StatusForbidden, why)
		return
	}
	if _, err := s.sessionAccount(sess); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var body struct {
		ProjectID string `json:"project_id"`
		Kind      string `json:"kind"`
	}
	if err := decodeClientRequest(w, r, &body); err != nil {
		writeClientError(w, err)
		return
	}
	var terminal store.ClientTerminal
	err := s.workspaces().WithSession(r.Context(), sess.ID, func(current store.Session) error {
		project, err := s.store.ClientProject(current.ID, body.ProjectID)
		if err != nil {
			return err
		}
		if err = s.checkClientProjectDir(current, project.Path); err != nil {
			return err
		}
		terminal, err = s.store.CreateClientTerminal(current.ID, project.ID, body.Kind)
		return err
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, terminal)
}

func (s *Server) handleClientTerminalDelete(w http.ResponseWriter, r *http.Request, sess store.Session) {
	id := r.PathValue("terminal")
	if !store.ValidClientResourceID(id) {
		writeClientError(w, store.ErrClientInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	err := s.workspaces().WithSession(ctx, sess.ID, func(current store.Session) error {
		if err := s.store.BeginCloseClientTerminal(current.ID, id); errors.Is(err, store.ErrClientMissing) {
			return nil
		} else if err != nil {
			return err
		}
		// Termination is cleanup, like stopping a workspace: allow it after account
		// revocation and never inject credentials or start a stopped container.
		if current.ContainerID != "" {
			running, err := s.dock.Running(ctx, current.ContainerID)
			if err != nil {
				return err
			}
			if running {
				_, err = s.dock.ExecCommand(ctx, current.ContainerID, []string{"timeout", "-k", "2", "10", "/bin/bash", "-c", clientTerminalStopCommand(id)})
				if err != nil {
					return err
				}
			}
		}
		return s.store.DeleteClientTerminal(current.ID, id)
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
