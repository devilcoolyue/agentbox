package server

import (
	"errors"
	"io"
	"log"
	"net/http"

	"agentbox/internal/store"
	"agentbox/internal/theme"
)

// 界面主题：全站主题由管理员导入、所有登录用户可选；个人主题只给本人用。
// 清单的校验规则（令牌白名单、安全取值语法）都在 internal/theme，这里只管
// 范围与鉴权：全站写操作挂 s.admin，个人范围永远取请求者自己的用户名。

func (s *Server) themeService() *theme.Service {
	s.themesOnce.Do(func() { s.themes = theme.New(s.cfg.DataDir) })
	return s.themes
}

type themeView struct {
	Scope     string         `json:"scope"`
	Manifest  theme.Manifest `json:"manifest"`
	UpdatedAt int64          `json:"updated_at"`
}

func themeViews(scope string, entries []theme.Entry) []themeView {
	out := make([]themeView, 0, len(entries))
	for _, e := range entries {
		out = append(out, themeView{Scope: scope, Manifest: e.Manifest, UpdatedAt: e.UpdatedAt.UnixMilli()})
	}
	return out
}

// themeError 返回中文说明（旧前端直接显示）与稳定的 theme_error，前端按 code 翻译。
func themeError(w http.ResponseWriter, status int, ve *theme.ValidationError) {
	writeJSON(w, status, map[string]any{"error": ve.Error(), "theme_error": ve})
}

func (s *Server) handleThemes(w http.ResponseWriter, r *http.Request) {
	u := reqUser(r)
	svc := s.themeService()
	site, err := svc.List("")
	if err != nil {
		log.Printf("theme: list site themes: %v", err)
		writeErr(w, http.StatusInternalServerError, "无法读取全站主题")
		return
	}
	mine, err := svc.List(u.Name)
	if err != nil {
		log.Printf("theme: list themes of %s: %v", u.Name, err)
		writeErr(w, http.StatusInternalServerError, "无法读取个人主题")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"format":          theme.FormatVersion,
		"bases":           theme.Bases,
		"tokens":          theme.Tokens(),
		"site":            themeViews("site", site),
		"user":            themeViews("user", mine),
		"can_manage_site": u.Role == store.RoleAdmin,
		"limits":          map[string]int{"site": theme.MaxSite, "user": theme.MaxUser, "bytes": theme.MaxBytes},
	})
}

func (s *Server) handleSiteThemePut(w http.ResponseWriter, r *http.Request) { s.putTheme(w, r, "site", "") }
func (s *Server) handleUserThemePut(w http.ResponseWriter, r *http.Request) {
	s.putTheme(w, r, "user", reqUser(r).Name)
}
func (s *Server) handleSiteThemeDelete(w http.ResponseWriter, r *http.Request) {
	s.deleteTheme(w, r, "")
}
func (s *Server) handleUserThemeDelete(w http.ResponseWriter, r *http.Request) {
	s.deleteTheme(w, r, reqUser(r).Name)
}

func (s *Server) putTheme(w http.ResponseWriter, r *http.Request, scope, owner string) {
	id := r.PathValue("id")
	if !theme.ValidID(id) {
		writeErr(w, http.StatusBadRequest, "主题 ID 无效")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, theme.MaxBytes+1))
	if err != nil || len(raw) > theme.MaxBytes {
		themeError(w, http.StatusRequestEntityTooLarge, theme.TooLarge())
		return
	}
	m, err := theme.Parse(raw)
	var ve *theme.ValidationError
	if errors.As(err, &ve) {
		themeError(w, http.StatusBadRequest, ve)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "主题文件无效")
		return
	}
	if m.ID != id {
		writeErr(w, http.StatusBadRequest, "地址里的主题 ID 与文件里的 id 不一致")
		return
	}
	entry, err := s.themeService().Put(owner, m)
	if errors.Is(err, theme.ErrLimit) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "主题数量已达上限", "theme_error": map[string]string{"code": "limit"}})
		return
	}
	if err != nil {
		log.Printf("theme: save %s theme %q: %v", scope, id, err)
		writeErr(w, http.StatusInternalServerError, "保存主题失败")
		return
	}
	writeJSON(w, http.StatusOK, themeView{Scope: scope, Manifest: entry.Manifest, UpdatedAt: entry.UpdatedAt.UnixMilli()})
}

func (s *Server) deleteTheme(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	if !theme.ValidID(id) {
		writeErr(w, http.StatusBadRequest, "主题 ID 无效")
		return
	}
	err := s.themeService().Delete(owner, id)
	if errors.Is(err, theme.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "主题不存在")
		return
	}
	if err != nil {
		log.Printf("theme: delete %q: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "删除主题失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
