package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"agentbox/internal/iconset"
)

// 全站图标：管理员把界面位置换成图标库里的其他图标。位置注册表与默认图标在前端
// （web/src/icon-slots.ts）；服务端只存覆盖表，并把用到的图标形状从随二进制分发
// 的图标库里取出来一并下发，浏览器不用为此加载整套图标库。

func (s *Server) iconService() *iconset.Service {
	s.iconsOnce.Do(func() { s.icons = iconset.New(s.cfg.DataDir, iconset.NewLibrary(webFS())) })
	return s.icons
}

func (s *Server) writeIcons(w http.ResponseWriter, cfg iconset.Config) {
	shapes, err := s.iconService().Library().Shapes(cfg)
	if err != nil {
		log.Printf("icons: load icon sets: %v", err)
		writeErr(w, http.StatusInternalServerError, "无法读取图标库")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"overrides":  cfg.Overrides,
		"icons":      shapes,
		"updated_at": cfg.UpdatedAt,
		"updated_by": cfg.UpdatedBy,
		"sets":       iconset.Sets,
		"limit":      iconset.MaxOverrides,
	})
}

func (s *Server) handleIcons(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.iconService().Get()
	if err != nil {
		// 配置读不出来时界面照常用默认图标，不让整个页面因此报错
		log.Printf("icons: %v", err)
	}
	s.writeIcons(w, cfg)
}

func (s *Server) handleIconsPut(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "图标配置过大")
		return
	}
	var body struct {
		Overrides map[string]string `json:"overrides"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "图标配置格式无效")
		return
	}
	cfg, err := s.iconService().Put(body.Overrides, reqUser(r).Name)
	var ve *iconset.ValidationError
	if errors.As(err, &ve) {
		writeErr(w, http.StatusBadRequest, ve.Msg)
		return
	}
	if err != nil {
		log.Printf("icons: save: %v", err)
		writeErr(w, http.StatusInternalServerError, "无法保存图标配置")
		return
	}
	s.writeIcons(w, cfg)
}
