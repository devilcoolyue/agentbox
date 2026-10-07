package server

import (
	"net/http"

	"agentbox/internal/config"
	"agentbox/internal/credentials"
	"agentbox/internal/store"
)

type onboardingAccount struct {
	ID                 string `json:"id"`
	Type               string `json:"type"`
	Label              string `json:"label"`
	CredentialsPresent bool   `json:"credentials_present"`
}

type onboardingView struct {
	Version            int                 `json:"version"`
	CanConfigure       bool                `json:"can_configure"`
	CanCreate          bool                `json:"can_create"`
	HasWorkspaces      bool                `json:"has_workspaces"`
	Accounts           []onboardingAccount `json:"accounts"`
	DefaultModels      map[string]string   `json:"default_models"`
	ContainerResources creationResources   `json:"container_resources"`
}

// Only public resource limits are projected. Network names, mounts and other
// administrator configuration must not leak through first-use guidance.
type creationResources struct {
	CPUs      float64 `json:"cpus"`
	MemoryMB  int64   `json:"memory_mb"`
	PidsLimit int64   `json:"pids_limit"`
}

func (s *Server) creationResources() creationResources {
	c := s.cfg.GetContainer()
	return creationResources{CPUs: c.CPUs, MemoryMB: c.MemoryMB, PidsLimit: c.PidsLimit}
}

// This is a read-only, actor-scoped configuration snapshot. Credential presence
// is guidance, not an authentication test or a new workspace admission rule.
func (s *Server) handleOnboarding(w http.ResponseWriter, r *http.Request) {
	u := reqUser(r)
	admin := u.Role == store.RoleAdmin
	view := onboardingView{Version: 1, CanConfigure: admin, HasWorkspaces: len(s.store.List(u.Name)) > 0,
		Accounts: []onboardingAccount{}, DefaultModels: map[string]string{}, ContainerResources: s.creationResources()}
	for _, acct := range s.cfg.AccountList() {
		if !acct.CanUse(u.Name, admin) || acct.Type != config.AgentClaude && acct.Type != config.AgentCodex {
			continue
		}
		view.Accounts = append(view.Accounts, onboardingAccount{acct.ID, acct.Type, acct.Label, credentials.ConfigurationPresent(acct)})
		view.DefaultModels[acct.Type] = s.cfg.GetDefaultModel(acct.Type)
	}
	if admin {
		for _, agent := range []string{config.AgentClaude, config.AgentCodex} {
			view.DefaultModels[agent] = s.cfg.GetDefaultModel(agent)
		}
	}
	view.CanCreate = len(view.Accounts) > 0
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, view)
}
