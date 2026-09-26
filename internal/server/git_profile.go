package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"
)

func validGitIdentity(name, email string) bool {
	if name == "" || len(name) > 128 || len(email) > 254 || !utf8.ValidString(name+email) {
		return false
	}
	for _, r := range name + email {
		if unicode.IsControl(r) || r == '<' || r == '>' {
			return false
		}
	}
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Address == email && addr.Name == "" && !strings.ContainsAny(email, " \t")
}

func (s *Server) handleGitProfile(w http.ResponseWriter, r *http.Request) {
	user := reqUser(r).Name
	if r.Method == http.MethodPut {
		var req struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeErr(w, 400, "请求体格式错误")
			return
		}
		if dec.Decode(&struct{}{}) != io.EOF {
			writeErr(w, 400, "请求体格式错误")
			return
		}
		req.Name, req.Email = strings.TrimSpace(req.Name), strings.TrimSpace(req.Email)
		if !validGitIdentity(req.Name, req.Email) {
			writeErr(w, 400, "请填写有效的提交姓名与邮箱；不能包含换行、控制字符或尖括号")
			return
		}
		profile, err := s.store.SetGitProfile(user, req.Name, req.Email)
		if err != nil {
			writeErr(w, 500, "保存 Git 身份失败")
			return
		}
		writeJSON(w, 200, profile)
		return
	}
	profile, err := s.store.GetGitProfile(user)
	if err != nil {
		writeErr(w, 500, "读取 Git 身份失败")
		return
	}
	writeJSON(w, 200, profile)
}
