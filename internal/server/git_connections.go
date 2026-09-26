package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentbox/internal/gitaccess"
	"agentbox/internal/store"
)

func (s *Server) gitVault() *gitaccess.Vault {
	s.gitVaultOnce.Do(func() { s.gitSecrets = &gitaccess.Vault{DataDir: s.cfg.DataDir} })
	return s.gitSecrets
}
func decodeGitJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(&struct{}{}) != io.EOF {
		writeErr(w, 400, "请求体格式错误")
		return false
	}
	return true
}
func writeGitStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, 404, "Git 连接或工作空间不存在或不可用")
	case errors.Is(err, store.ErrGitConflict), errors.Is(err, store.ErrGitInUse), errors.Is(err, store.ErrGitLimit):
		writeErr(w, 409, err.Error())
	case errors.Is(err, gitaccess.ErrKey), errors.Is(err, gitaccess.ErrSecret):
		writeErr(w, 503, err.Error())
	default:
		writeErr(w, 500, "Git 配置读写失败")
	}
}
func gitText(v string, max int, empty bool) bool {
	if !utf8.ValidString(v) || len(v) > max || (!empty && strings.TrimSpace(v) == "") {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func (s *Server) handleGitConnections(w http.ResponseWriter, r *http.Request) {
	user := reqUser(r).Name
	if r.Method == http.MethodGet {
		rows, err := s.store.GitConnectionsFor(user)
		if err != nil {
			writeGitStoreErr(w, err)
			return
		}
		for i := range rows {
			if rows[i].Managed && rows[i].AuthType == "ssh" {
				if c, err := s.store.GitConnection(user, rows[i].ID); err == nil {
					if raw, err := s.gitVault().Open(c.Secret, c.AssociatedData()); err == nil {
						if key, err := gitaccess.ParseSSHCredential(raw); err == nil {
							rows[i].HostFingerprint, _ = key.Fingerprint()
							rows[i].PublicKey, _ = key.PublicKey()
						}
					}
				}
			}

			if rows[i].Managed && rows[i].AuthType == "oauth" {
				if c, err := s.store.GitConnection(user, rows[i].ID); err == nil {
					if raw, err := s.gitVault().Open(c.Secret, c.AssociatedData()); err == nil {
						var credential gitOAuthCredential
						if json.Unmarshal(raw, &credential) == nil {
							rows[i].OAuthAppID = credential.AppID
						}
					}
				}
			}
		}
		writeJSON(w, 200, rows)
		return
	}
	var req struct {
		Label      string                  `json:"label"`
		Provider   string                  `json:"provider"`
		BaseURL    string                  `json:"base_url"`
		Username   string                  `json:"username"`
		Token      string                  `json:"token"`
		AuthType   string                  `json:"auth_type"`
		PrivateKey string                  `json:"private_key"`
		Passphrase string                  `json:"passphrase"`
		HostKey    string                  `json:"host_key"`
		Network    gitaccess.NetworkPolicy `json:"network"`
		ReadOnly   *bool                   `json:"read_only"`
	}
	if !decodeGitJSON(w, r, &req) {
		return
	}
	if err := req.Network.Validate(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	req.Label, req.Username = strings.TrimSpace(req.Label), strings.TrimSpace(req.Username)
	if !gitText(req.Label, 128, false) || !gitText(req.Username, 256, true) || (req.AuthType != "ssh" && !gitText(req.Token, 16384, false)) {
		writeErr(w, 400, "连接名称、用户名或 Token 无效")
		return
	}
	if req.Provider != "github" && req.Provider != "gitlab" && req.Provider != "generic" {
		writeErr(w, 400, "请选择 GitHub、GitLab 或通用 Git")
		return
	}
	if req.AuthType == "" {
		req.AuthType = "pat"
	}
	if req.AuthType != "pat" && req.AuthType != "ssh" {
		writeErr(w, 400, "认证方式必须为 pat 或 ssh")
		return
	}
	var base string
	var err error
	if req.AuthType == "ssh" {
		if req.Username == "" {
			req.Username = "git"
		}
		base, err = gitaccess.SSHBaseURL(req.BaseURL, req.Username)
	} else {
		base, err = gitaccess.BaseURL(req.BaseURL)
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Username == "" {
		switch req.Provider {
		case "github":
			req.Username = "x-access-token"
		case "gitlab":
			req.Username = "oauth2"
		default:
			writeErr(w, 400, "通用 HTTPS Git 连接需要用户名")
			return
		}
	}
	c := store.GitConnection{ID: store.NewID() + store.NewID(), Owner: user, Label: req.Label, Provider: req.Provider, BaseURL: base, AuthType: req.AuthType, Network: req.Network, Username: req.Username, Enabled: true, ReadOnly: true}
	if req.ReadOnly != nil {
		c.ReadOnly = *req.ReadOnly
	}
	count, err := s.store.GitConnectionCount()
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	secret := []byte(req.Token)
	if c.AuthType == "ssh" {
		credential := gitaccess.SSHCredential{PrivateKey: req.PrivateKey, Passphrase: req.Passphrase, HostKey: req.HostKey}
		if _, err = credential.Signer(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if _, err = credential.Fingerprint(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		secret, _ = json.Marshal(credential)
	}
	c.Secret, err = s.gitVault().Seal(secret, c.AssociatedData(), count == 0 && len(s.cfg.GitOAuthAppList()) == 0)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	c, err = s.store.SaveGitConnectionWithLogin(c, 0, bearerToken(r))
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	writeJSON(w, 201, c)
}
func (s *Server) handleGitConnection(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GitConnection(reqUser(r).Name, r.PathValue("connection"))
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	if r.Method == http.MethodDelete {
		var req struct {
			Revision int64 `json:"revision"`
		}
		if !decodeGitJSON(w, r, &req) {
			return
		}
		if req.Revision < 1 {
			writeErr(w, 400, "需要连接修订号")
			return
		}
		if err := s.store.DeleteGitConnection(c.Owner, c.ID, req.Revision); err != nil {
			writeGitStoreErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"deleted": true})
		return
	}
	var req struct {
		Revision   int64   `json:"revision"`
		Label      *string `json:"label"`
		Token      *string `json:"token"`
		ReadOnly   *bool   `json:"read_only"`
		Enabled    *bool   `json:"enabled"`
		PrivateKey *string `json:"private_key"`
		Passphrase *string `json:"passphrase"`
		HostKey    *string `json:"host_key"`
	}
	if !decodeGitJSON(w, r, &req) {
		return
	}
	if req.Revision != c.Revision {
		writeGitStoreErr(w, store.ErrGitConflict)
		return
	}
	if req.Label != nil {
		c.Label = strings.TrimSpace(*req.Label)
		if !gitText(c.Label, 128, false) {
			writeErr(w, 400, "连接名称无效")
			return
		}
	}
	if req.Token != nil {
		if c.AuthType != "pat" {
			writeErr(w, 400, "OAuth 连接请重新授权，不能替换为手工 Token")
			return
		}
		if !gitText(*req.Token, 16384, false) {
			writeErr(w, 400, "Token 无效")
			return
		}
		c.Secret, err = s.gitVault().Seal([]byte(*req.Token), c.AssociatedData(), false)
		if err != nil {
			writeGitStoreErr(w, err)
			return
		}
	}
	if req.PrivateKey != nil || req.Passphrase != nil || req.HostKey != nil {
		if c.AuthType != "ssh" {
			writeErr(w, 400, "此连接不是 SSH 认证")
			return
		}
		raw, e := s.gitVault().Open(c.Secret, c.AssociatedData())
		if e != nil {
			writeGitStoreErr(w, e)
			return
		}
		key, e := gitaccess.ParseSSHCredential(raw)
		if e != nil {
			writeErr(w, 400, e.Error())
			return
		}
		if req.PrivateKey != nil {
			key.PrivateKey = *req.PrivateKey
			key.Passphrase = ""
		}
		if req.Passphrase != nil {
			key.Passphrase = *req.Passphrase
		}
		if req.HostKey != nil {
			key.HostKey = *req.HostKey
		}
		if _, e = key.Signer(); e != nil {
			writeErr(w, 400, e.Error())
			return
		}
		if _, e = key.Fingerprint(); e != nil {
			writeErr(w, 400, e.Error())
			return
		}
		encoded, _ := json.Marshal(key)
		c.Secret, err = s.gitVault().Seal(encoded, c.AssociatedData(), false)
		if err != nil {
			writeGitStoreErr(w, err)
			return
		}
	}
	if req.ReadOnly != nil {
		c.ReadOnly = *req.ReadOnly
	}
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	c, err = s.store.SaveGitConnectionWithLogin(c, req.Revision, bearerToken(r))
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	writeJSON(w, 200, c)
}
func (s *Server) handleGitDefault(w http.ResponseWriter, r *http.Request) { s.gitDefault(w, r, "") }
func (s *Server) handleSessionGitDefault(w http.ResponseWriter, r *http.Request, sess store.Session) {
	s.gitDefault(w, r, sess.ID)
}
func (s *Server) gitDefault(w http.ResponseWriter, r *http.Request, session string) {
	user := reqUser(r).Name
	if r.Method == http.MethodPut {
		var req struct {
			ConnectionID string `json:"connection_id"`
		}
		if !decodeGitJSON(w, r, &req) {
			return
		}
		if err := s.store.SetGitDefault(user, session, req.ConnectionID); err != nil {
			writeGitStoreErr(w, err)
			return
		}
	}
	id, err := s.store.GitDefault(user, session)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"connection_id": id})
}

var gitRemoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (s *Server) handleGitBindings(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if r.Method == http.MethodPut {
		var req struct {
			Repo         string `json:"repo"`
			Remote       string `json:"remote"`
			ConnectionID string `json:"connection_id"`
			Revision     int64  `json:"revision"`
		}
		if !decodeGitJSON(w, r, &req) {
			return
		}
		if !gitRemoteName.MatchString(req.Remote) || req.Revision < 0 {
			writeErr(w, 400, "remote 名称或修订号无效")
			return
		}
		b := store.GitBinding{SessionID: sess.ID, Repo: req.Repo, Remote: req.Remote, ConnectionID: req.ConnectionID}
		// Unbinding must work even after the remote or repository has been removed.
		if req.ConnectionID != "" {
			c, err := s.store.GitConnectionFor(sess.User, req.ConnectionID)
			if err != nil {
				writeGitStoreErr(w, err)
				return
			}
			if !c.Enabled {
				writeErr(w, 409, "连接已停用")
				return
			}
			dir, ok := s.gitRepo(w, sess, req.Repo)
			if !ok {
				return
			}
			rel, err := filepath.Rel(s.workspaceDir(sess), dir)
			if err != nil {
				writeGitStoreErr(w, err)
				return
			}
			if rel == "." {
				rel = ""
			}
			b.Repo = filepath.ToSlash(rel)
			ctx, cancel := gitCtx(r)
			defer cancel()
			unlock, err := s.lockGit(ctx, sess, dir)
			if err != nil {
				writeGitErr(w, err)
				return
			}
			defer unlock()
			raw, err := s.runGit(ctx, sess, dir, "config", "--local", "--null", "--get-all", "remote."+req.Remote+".url")
			if err != nil {
				writeErr(w, 400, "无法读取 remote 地址，请先在仓库中配置远程地址")
				return
			}
			urls := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
			if len(urls) != 1 {
				writeErr(w, 400, "此 remote 配置了多个地址，请先整理为一个明确目标")
				return
			}
			b.URL, err = gitRepositoryURL(urls[0], c)
			if err != nil {
				writeErr(w, 400, err.Error())
				return
			}
		}
		if err := s.store.SaveGitBinding(sess.User, b, req.Revision); err != nil {
			writeGitStoreErr(w, err)
			return
		}
	}
	rows, err := s.store.GitBindings(sess.ID)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
