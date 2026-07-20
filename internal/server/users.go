// 用户与登录：账号密码换取会话令牌（存 SQLite），管理员可增删普通用户。
// 首个启动时用 config.json 的 auth_token 作为 boxadmin 的初始密码建号，
// 老部署升级后用原令牌当密码登录即可。
package server

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentbox/internal/store"
)

const adminUser = "boxadmin"

var userNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)

// --- 密码哈希（PBKDF2-SHA256，格式 pbkdf2-sha256$iter$salt$key） ---

const pbkdf2Iter = 210_000

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, hex.EncodeToString(salt), hex.EncodeToString(key))
}

func verifyPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

const tokenChars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = tokenChars[int(b[i])%len(tokenChars)]
	}
	return string(b)
}

// seedUsers 空用户表时建管理员，并把单用户时代的会话与数据目录归到其名下。
func (s *Server) seedUsers() error {
	if s.store.CountUsers() > 0 {
		return nil
	}
	err := s.store.CreateUser(store.User{
		Name: adminUser, Role: store.RoleAdmin,
		PassHash: hashPassword(s.cfg.GetAuthToken()),
	})
	if err != nil {
		return err
	}
	if err := s.store.ReassignUser(legacyUser, adminUser); err != nil {
		return err
	}
	migrateLegacyUserDir(s.cfg.DataDir)
	log.Printf("seeded admin user %q (初始密码 = config.json 里的 auth_token)", adminUser)
	return nil
}

// --- 请求身份 ---

type ctxKey int

const ctxUser ctxKey = iota

func reqUser(r *http.Request) store.User {
	u, _ := r.Context().Value(ctxUser).(store.User)
	return u
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.store.TokenUser(bearerToken(r))
		if !ok {
			writeErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser, u)))
	})
}

// admin 在 auth 之上再要求管理员角色；设置类接口全部走这层。
func (s *Server) admin(next http.Handler) http.Handler {
	return s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqUser(r).Role != store.RoleAdmin {
			writeErr(w, http.StatusForbidden, "需要管理员权限")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// --- 登录 / 登出 ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	u, ok := s.store.GetUser(strings.TrimSpace(req.Username))
	if !ok || !verifyPassword(u.PassHash, req.Password) {
		time.Sleep(400 * time.Millisecond) // 拖慢在线爆破
		writeErr(w, http.StatusUnauthorized, "账号或密码错误")
		return
	}
	tok := newToken()
	if err := s.store.CreateToken(tok, u.Name); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok, "user": u.Name, "role": u.Role})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteToken(bearerToken(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleChangePassword 当前用户改自己的密码；其他端的令牌全部吊销，本端保持登录。
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	u := reqUser(r)
	if !verifyPassword(u.PassHash, req.OldPassword) {
		writeErr(w, http.StatusForbidden, "当前密码不正确")
		return
	}
	if len(req.NewPassword) < 8 {
		writeErr(w, http.StatusBadRequest, "新密码至少 8 位")
		return
	}
	if err := s.store.SetPassword(u.Name, hashPassword(req.NewPassword)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.DeleteUserTokensExcept(u.Name, bearerToken(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- 用户管理（管理员） ---

type userView struct {
	Name      string `json:"name"`
	Role      string `json:"role"`
	Sessions  int    `json:"sessions"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Server) handleUserList(w http.ResponseWriter, r *http.Request) {
	counts := s.store.SessionCounts()
	out := []userView{}
	for _, u := range s.store.ListUsers() {
		out = append(out, userView{
			Name: u.Name, Role: u.Role,
			Sessions: counts[u.Name], CreatedAt: u.CreatedAt.UnixMilli(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !userNameRe.MatchString(req.Username) {
		writeErr(w, http.StatusBadRequest, "用户名不合法：小写字母或数字开头，可含 - 和 _，长度 2-32")
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "密码至少 8 位")
		return
	}
	if _, exists := s.store.GetUser(req.Username); exists {
		writeErr(w, http.StatusConflict, "用户名已存在")
		return
	}
	u := store.User{Name: req.Username, Role: store.RoleUser, PassHash: hashPassword(req.Password)}
	if err := s.store.CreateUser(u); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, userView{Name: u.Name, Role: u.Role, CreatedAt: time.Now().UnixMilli()})
}

// handleUserSetPassword 管理员重置任意用户密码；该用户其他端全部下线
//（重置自己时当前端保持登录）。
func (s *Server) handleUserSetPassword(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.store.GetUser(name); !ok {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "密码至少 8 位")
		return
	}
	if err := s.store.SetPassword(name, hashPassword(req.Password)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	keep := ""
	if name == reqUser(r).Name {
		keep = bearerToken(r)
	}
	if err := s.store.DeleteUserTokensExcept(name, keep); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUserDelete 删除普通用户并级联删除其全部会话容器；工作区文件
// 保留在 data/users/<name> 下，由管理员自行清理。
func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	u, ok := s.store.GetUser(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if u.Role == store.RoleAdmin {
		writeErr(w, http.StatusBadRequest, "不能删除管理员账号")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	for _, sess := range s.store.List(name) {
		if sess.ContainerID != "" {
			if err := s.dock.Remove(ctx, sess.ContainerID); err != nil {
				log.Printf("delete user %s: remove container of %s: %v", name, sess.ID, err)
			}
		}
		if err := s.store.Delete(sess.ID); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := s.store.DeleteUser(name); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
