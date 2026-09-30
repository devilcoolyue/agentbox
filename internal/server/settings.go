// 系统设置 API：账号池的增删改 + 服务端配置的读写。所有改动经
// config.Config 的校验后写回 config.json；除监听地址外均即时生效。
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"agentbox/internal/buildinfo"
	"agentbox/internal/config"
	"agentbox/internal/store"
)

// --- 设置读写 ---

type settingsView struct {
	ImageUpdates   config.ImageUpdateConfig        `json:"image_updates"`
	Resources      config.ResourceLimits           `json:"resources"`
	Listen         string                          `json:"listen"`
	AgentImage     string                          `json:"agent_image"`
	PermissionMode string                          `json:"permission_mode"`
	MaxUploadMB    int64                           `json:"max_upload_mb"`
	IdleTimeoutMin int64                           `json:"idle_timeout_min"`
	TimeZone       string                          `json:"timezone"`
	Container      config.ContainerLimits          `json:"container"`
	Models         map[string][]config.ModelOption `json:"models"`
	DefaultModels  map[string]string               `json:"default_models"`
	TerminalTips   config.TerminalTips             `json:"terminal_tips"`
	Tunnel         config.TunnelConfig             `json:"tunnel"`
	TunnelActive   bool                            `json:"tunnel_active"`          // SOCKS 代理是否真的监听中
	TunnelError    string                          `json:"tunnel_error,omitempty"` // 最近一次启停失败的原因
	ProxyBridge    config.ProxyBridgeConfig        `json:"proxy_bridge"`
	// Pricing 是「按 token 折算费用」的价目表，键是模型 id 或 agent 名。
	// 只发给管理员——设置页本来就是 admin 才进得来。
	Pricing         map[string]config.ModelPrice `json:"pricing"`
	RestartRequired bool                         `json:"restart_required"`
}

func (s *Server) settingsView() settingsView {
	policy, _, _ := s.cfg.ImageUpdateState()
	return settingsView{
		ImageUpdates:    policy,
		Resources:       s.cfg.GetResources(),
		Listen:          s.cfg.GetListen(),
		AgentImage:      s.cfg.GetAgentImage(),
		PermissionMode:  s.cfg.GetPermissionMode(),
		MaxUploadMB:     s.cfg.GetMaxUploadMB(),
		IdleTimeoutMin:  s.cfg.GetIdleTimeoutMin(),
		TimeZone:        s.cfg.GetTimeZone(),
		Container:       s.cfg.GetContainer(),
		Models:          s.cfg.GetModels(),
		DefaultModels:   s.cfg.GetDefaultModels(),
		TerminalTips:    s.cfg.GetTerminalTips(),
		Tunnel:          s.cfg.GetTunnel(),
		TunnelActive:    s.tunnels.proxyUp.Load(),
		ProxyBridge:     s.cfg.GetProxyBridge(),
		Pricing:         s.cfg.GetPricing(),
		RestartRequired: s.cfg.GetListen() != s.bootListen,
	}
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsView())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var patch config.SettingsPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if patch.Tunnel != nil && s.cfg.GetTunnel().Transparent && !patch.Tunnel.Transparent {
		for _, sess := range s.store.All() {
			if sess.Status == store.StatusRunning {
				writeErr(w, 409, "切换回兼容代理模式前，请先停止运行中的工作空间")
				return
			}
		}
	}
	if err := s.cfg.ApplySettings(patch); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	view := s.settingsView()
	if patch.Tunnel != nil {
		// 隧道开关热生效：配置已落盘，这里同步启停 SOCKS 代理；绑定失败不算
		// 保存失败，把原因回给前端展示。
		if err := s.applyTunnel(); err != nil {
			view.TunnelError = err.Error()
		}
		if err := s.applyNetwork(); err != nil {
			view.TunnelError = err.Error()
		}
		view.TunnelActive = s.tunnels.proxyUp.Load()
	}
	if patch.ProxyBridge != nil {
		// 换绑定地址同样热生效：重绑失败只记日志，代理列表接口里会显示当前状态。
		s.refreshProxyBridge()
	}
	writeJSON(w, http.StatusOK, view)
}

// --- 系统信息（关于页） ---

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	total, running := 0, 0
	for _, sess := range s.store.All() {
		total++
		if sess.Status == "running" {
			running++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"go_version": runtime.Version(),
		"version":    buildinfo.Version, "revision": buildinfo.Commit(), "built_at": buildinfo.BuiltAt, "schema_version": store.SchemaVersion,
		"data_dir":         s.cfg.DataDir,
		"config_path":      s.cfg.Path(),
		"docker_version":   s.dock.ServerVersion(r.Context()),
		"sessions_total":   total,
		"sessions_running": running,
		"accounts":         len(s.cfg.AccountList()),
		"users":            s.store.CountUsers(),
		"started_at":       s.startedAt.UnixMilli(),
		"listen":           s.bootListen,
	})
}

// --- 账号池维护 ---

// handleAccountCreate 新建账号：凭证目录自动建在 data/creds/<id> 下，
// 创建后前端随即引导进入登录（OAuth / API Key）流程。
func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID             string                                 `json:"id"`
		Type           string                                 `json:"type"`
		Label          string                                 `json:"label"`
		Env            map[string]string                      `json:"env"`
		ProxyID        string                                 `json:"proxy_id"`
		Access         *config.AccountAccess                  `json:"access"`
		ModelReasoning *map[string]config.ReasoningCapability `json:"model_reasoning"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.Label = strings.TrimSpace(req.Label)
	if !config.ValidAccountID(req.ID) {
		writeErr(w, http.StatusBadRequest, "账号 ID 不合法：小写字母或数字开头，可含 - 和 _，长度 2-32")
		return
	}
	if req.Type != config.AgentClaude && req.Type != config.AgentCodex {
		writeErr(w, http.StatusBadRequest, "账号类型必须是 claude 或 codex")
		return
	}
	if req.Label == "" {
		req.Label = req.ID
	}
	if err := s.validateAccountAccess(req.Access); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	credDir := filepath.Join(s.cfg.DataDir, "creds", req.ID)
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	acct := config.Account{
		ID: req.ID, Type: req.Type, Label: req.Label,
		CredentialsDir: credDir, Env: req.Env, Access: req.Access, ProxyID: req.ProxyID,
	}
	if req.ModelReasoning != nil {
		acct.ModelReasoning = *req.ModelReasoning
	}
	if err := s.cfg.AddAccount(acct); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.accountView(acct, 0))
}

func (s *Server) handleAccountPatch(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.cfg.Account(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var req struct {
		Label          *string                                `json:"label"`
		Env            *map[string]string                     `json:"env"`
		ProxyID        *string                                `json:"proxy_id"`
		Access         *config.AccountAccess                  `json:"access"`
		ModelReasoning *map[string]config.ReasoningCapability `json:"model_reasoning"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if err := s.validateAccountAccess(req.Access); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	patch := config.AccountPatch{Access: req.Access, ModelReasoning: req.ModelReasoning}
	if req.Label != nil {
		label := strings.TrimSpace(*req.Label)
		if label == "" {
			label = acct.ID
		}
		patch.Label = &label
	}
	if req.Env != nil {
		env := *req.Env
		if len(env) == 0 {
			env = nil
		}
		patch.Env = &env
	}
	if req.ProxyID != nil {
		pid := strings.TrimSpace(*req.ProxyID)
		patch.ProxyID = &pid
	}
	updated, err := s.cfg.UpdateAccount(acct.ID, patch)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	counts := s.accountSessionCounts()
	writeJSON(w, http.StatusOK, s.accountView(updated, counts[updated.ID]))
}

// handleAccountDelete 删除账号：有会话在用时拒绝；凭证目录保留在磁盘上
// （里面是登录凭证，误删不可恢复，留给管理员自行清理）。
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.cfg.Account(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if n := s.accountSessionCounts()[acct.ID]; n > 0 {
		writeErr(w, http.StatusConflict, "仍有 "+strconv.Itoa(n)+" 个工作空间在使用该账号，请先删除这些工作空间")
		return
	}
	if err := s.cfg.RemoveAccount(acct.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
