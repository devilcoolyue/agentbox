package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"

	"agentbox/internal/buildinfo"
	"agentbox/internal/config"
	"agentbox/internal/extension"
	"agentbox/internal/extension/builtin"
	"agentbox/pkg/extspi"
)

// Extensions (docs/architecture/extensions-spi.md): the host runs the enabled
// extensions and dispatches hooks to them; this file wires it to the server
// and serves the admin API. Hooks on model traffic additionally need the model
// gateway (modelgateway.go).

func newExtensionHost() (*extension.Host, error) {
	return extension.NewHost(extension.Options{
		HostVersion: buildinfo.Version,
		Builtins:    builtin.All(),
		OnEvent: func(e extension.Event) {
			log.Printf("extension %s: %s %s session=%s account=%s", e.Extension, e.Kind, e.Code, e.Session, e.Account)
		},
	})
}

func (s *Server) initExtensions() error {
	host, err := newExtensionHost()
	if err != nil {
		return err
	}
	s.extensions = host
	return s.initModelGateway()
}

// applyExtensions reconciles extension instances, and the model gateway that
// model hooks depend on, with the stored configuration.
func (s *Server) applyExtensions() {
	if s.extensions == nil {
		return // servers built without NewContext (tests) have no host
	}
	s.extensions.Apply(s.cfg.GetExtensions().Items)
	if err := s.applyModelGateway(); err != nil {
		log.Printf("model gateway disabled: %v", err)
	}
	// Codex endpoints live in files; bring them in line off the request path.
	s.spawn(func() { s.reconcileCodexEndpoints(s.workContext()) })
}

// --- admin API ---

type extensionView struct {
	ID            string                   `json:"id"`
	Version       string                   `json:"version"`
	Publisher     string                   `json:"publisher"`
	Name          extension.Text           `json:"name"`
	Description   extension.Text           `json:"description"`
	License       string                   `json:"license,omitempty"`
	Homepage      string                   `json:"homepage,omitempty"`
	Builtin       bool                     `json:"builtin"`
	Runtime       string                   `json:"runtime"`
	Hooks         []string                 `json:"hooks"`
	Permissions   []string                 `json:"permissions"`
	FailurePolicy string                   `json:"failure_policy"`
	ConfigSchema  extension.ConfigSchema   `json:"config_schema"`
	Errors        []extension.ErrorDef     `json:"errors"`
	Metrics       []extension.MetricDef    `json:"metrics"`
	Setting       config.ExtensionSetting  `json:"setting"`
	Status        extension.StatusView     `json:"status"`
	Accounts      []extensionAccountChoice `json:"accounts"`
	// Requires lists the infrastructure the extension's hooks run on, so its
	// page can show the state without making it part of every extension.
	Requires []extensionRequirement `json:"requires"`
}

// extensionRequirement is infrastructure an extension depends on; only the
// model gateway exists today (id model_gateway, for the model.* hooks).
type extensionRequirement struct {
	ID     string `json:"id"`
	Bind   string `json:"bind"`
	Active bool   `json:"active"`
	Error  string `json:"error,omitempty"`
}

type extensionAccountChoice struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Eligible bool   `json:"eligible"`
}

type extensionsResponse struct {
	Extensions []extensionView `json:"extensions"`
}

func (s *Server) extensionView(p extension.Package, items map[string]config.ExtensionSetting) extensionView {
	m := p.Manifest
	setting, ok := items[m.ID]
	if !ok {
		// Not configured yet: show the manifest defaults the form starts from.
		setting = config.ExtensionSetting{Permissions: append([]string(nil), m.Permissions...), Bindings: config.ExtensionBindings{Mode: "none"}}
		if n, err := s.extensions.Normalize(m.ID, setting); err == nil {
			setting = n
		}
	}
	if setting.Permissions == nil {
		setting.Permissions = []string{}
	}
	v := extensionView{ID: m.ID, Version: m.Version, Publisher: m.Publisher, Name: m.Name, Description: m.Description, License: m.License,
		Homepage: m.Homepage, Builtin: p.Builtin, Runtime: m.Runtime.Type, Hooks: m.Hooks, Permissions: m.Permissions,
		FailurePolicy: m.FailurePolicy, ConfigSchema: m.ConfigSchema, Errors: m.Errors, Metrics: m.Status.Metrics,
		Setting: setting, Status: s.extensions.Status(m.ID), Accounts: []extensionAccountChoice{}, Requires: []extensionRequirement{}}
	if slices.ContainsFunc(modelHooks, m.HasHook) {
		gw := s.modelGatewayView()
		v.Requires = append(v.Requires, extensionRequirement{ID: "model_gateway", Bind: gw.Bind, Active: gw.Active, Error: gw.Error})
	}
	if v.Metrics == nil {
		v.Metrics = []extension.MetricDef{}
	}
	for _, a := range s.cfg.AccountList() {
		v.Accounts = append(v.Accounts, extensionAccountChoice{ID: a.ID, Label: a.Label, Type: a.Type, Eligible: gatewayEligible(a)})
	}
	return v
}

func (s *Server) extensionsResponse() extensionsResponse {
	ext := s.cfg.GetExtensions()
	out := extensionsResponse{Extensions: []extensionView{}}
	for _, p := range s.extensions.Packages() {
		out.Extensions = append(out.Extensions, s.extensionView(p, ext.Items))
	}
	return out
}

func (s *Server) handleExtensions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.extensionsResponse())
}

func (s *Server) handleExtension(w http.ResponseWriter, r *http.Request) {
	p, ok := s.extensions.Package(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "extension not found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.extensionView(p, s.cfg.GetExtensions().Items))
}

type extensionUpdate struct {
	ExpectedRevision *int64                   `json:"expected_revision"`
	Enabled          bool                     `json:"enabled"`
	Order            int                      `json:"order"`
	Config           json.RawMessage          `json:"config"`
	Permissions      []string                 `json:"permissions"`
	Bindings         config.ExtensionBindings `json:"bindings"`
	FailurePolicy    string                   `json:"failure_policy"`
}

func (s *Server) handleUpdateExtension(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.extensions.Package(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "extension not found")
		return
	}
	var req extensionUpdate
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ExpectedRevision == nil {
		writeErr(w, http.StatusBadRequest, "expected_revision is required")
		return
	}
	setting, err := s.extensions.Normalize(id, config.ExtensionSetting{Enabled: req.Enabled, Order: req.Order, Config: req.Config,
		Permissions: req.Permissions, Bindings: req.Bindings, FailurePolicy: req.FailurePolicy})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if setting.Enabled {
		for _, hook := range p.Manifest.Hooks {
			if need := extspi.HookPermission[hook]; !slices.Contains(setting.Permissions, need) {
				writeErr(w, http.StatusBadRequest, "启用前需要批准扩展运行所需的权限："+need)
				return
			}
		}
	}
	if _, err := s.cfg.SetExtension(id, setting, *req.ExpectedRevision); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, config.ErrExtensionRevision) {
			status = http.StatusConflict
		}
		writeErr(w, status, err.Error())
		return
	}
	s.applyExtensions()
	writeJSON(w, http.StatusOK, s.extensionView(p, s.cfg.GetExtensions().Items))
}

func (s *Server) handleExtensionLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.extensions.Package(id); !ok {
		writeErr(w, http.StatusNotFound, "extension not found")
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.extensions.Logs(id, since)})
}

func (s *Server) handleExtensionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.extensions.Package(id); !ok {
		writeErr(w, http.StatusNotFound, "extension not found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"events": s.extensions.Events(id)})
}
