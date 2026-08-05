package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"agentbox/internal/archivex"
	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

// 技能页后端：Claude Code 的 skill 目录管理，两个范围
//
//	session   会话 home 的 ~/.claude/skills —— 只影响这一个会话
//	template  用户模板 data/users/<user>/home-template/.claude/skills ——
//	          每次会话启动铺进该用户的所有会话（见 agent.SeedHomeTemplate）
//
// 服务器级模板（data/home-template）刻意不在这里开放：那是全体用户可见的，
// 归管理员在宿主机上维护。
const (
	skillsSubPath   = ".claude/skills"
	skillManifest   = "SKILL.md"
	skillMaxRead    = 256 << 10 // 单个文件的文本读取上限，超出截断
	skillMaxEntries = 2000      // 文件树里最多列出多少个条目（含子目录）
)

// 技能名同时是目录名：限定为文件名安全的字符，杜绝路径穿越。
var skillNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var errSkillScope = errors.New("invalid scope")

func (s *Server) skillsRoot(scope string, sess store.Session) (string, error) {
	switch scope {
	case "", "session":
		return filepath.Join(s.homeDir(sess), filepath.FromSlash(skillsSubPath)), nil
	case "template":
		return filepath.Join(s.userTemplateDir(sess.User), filepath.FromSlash(skillsSubPath)), nil
	default:
		return "", errSkillScope
	}
}

type skillInfo struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Files       int       `json:"files"`
	Bytes       int64     `json:"bytes"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Source 只在 session 范围有意义：这个技能是会话自己装的（session），
	// 还是来自用户模板（template）/ 服务器模板（global）。
	Source string `json:"source"`
}

type skillDetail struct {
	skillInfo
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	// Entries 是整个技能目录的扁平清单（含子目录、含 SKILL.md 自己），
	// 前端据此拼出左侧文件树。父目录一定排在自己的子项之前。
	Entries []skillEntry `json:"entries"`
	More    bool         `json:"more"` // 条目太多，清单被截断
}

// skillEntry 是技能目录里的一个文件或子目录，路径相对技能目录。
type skillEntry struct {
	Path  string    `json:"path"`
	Dir   bool      `json:"dir,omitempty"`
	Link  bool      `json:"link,omitempty"` // 符号链接：装技能时不会产生，会话里可能自己造
	Exec  bool      `json:"exec,omitempty"` // scripts/ 下的脚本有没有可执行位
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// skillFile 是技能目录里某一个文件的内容，供文件树的右侧预览。
type skillFile struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	Mode      string    `json:"mode"`
	Exec      bool      `json:"exec"`
	MTime     time.Time `json:"mtime"`
	Content   string    `json:"content"`
	Truncated bool      `json:"truncated"`
	Binary    bool      `json:"binary"` // 图标、字体之类：只报大小，内容留给 ?raw=1
}

func (s *Server) handleSkillList(w http.ResponseWriter, r *http.Request, sess store.Session) {
	scope := r.URL.Query().Get("scope")
	root, err := s.skillsRoot(scope, sess)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid scope")
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []skillInfo{}
	for _, e := range entries {
		if !e.IsDir() || !skillNameRe.MatchString(e.Name()) {
			continue
		}
		info := s.describeSkill(filepath.Join(root, e.Name()), e.Name())
		if scope == "template" {
			info.Source = "template"
		} else {
			info.Source = s.skillOrigin(sess, e.Name())
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

// skillOrigin 说明会话里这个技能是哪儿来的：用户模板 > 服务器模板 > 会话自装。
// 顺序与 SeedHomeTemplate 的分层一致。
func (s *Server) skillOrigin(sess store.Session, name string) string {
	for _, probe := range []struct{ dir, label string }{
		{s.userTemplateDir(sess.User), "template"},
		{s.homeTemplateDir(), "global"},
	} {
		p := filepath.Join(probe.dir, filepath.FromSlash(skillsSubPath), name)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return probe.label
		}
	}
	return "session"
}

// describeSkill 汇总一个技能目录：描述取自 SKILL.md 的 front matter，
// 大小/时间取整个目录。读不动的目录也返回条目，让前端能显示并删除它。
func (s *Server) describeSkill(dir, name string) skillInfo {
	info := skillInfo{Name: name}
	info.Description = skillDescription(filepath.Join(dir, skillManifest))
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		info.Files++
		info.Bytes += fi.Size()
		if fi.ModTime().After(info.UpdatedAt) {
			info.UpdatedAt = fi.ModTime()
		}
		return nil
	})
	return info
}

// skillDescription 从 SKILL.md 的 YAML front matter 里取 description。
// 只认最简单的 `key: value` 单行形式——技能文件都是这么写的，为此引一个
// YAML 依赖不划算；取不到就留空，前端显示「（无描述）」。
func skillDescription(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 8<<10)
	n, _ := io.ReadFull(f, head)
	text := strings.ReplaceAll(string(head[:n]), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return ""
	}
	body := text[len("---\n"):]
	if end := strings.Index(body, "\n---"); end >= 0 {
		body = body[:end]
	}
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(line, "description:")
		if !ok {
			continue
		}
		return strings.Trim(strings.TrimSpace(rest), `"'`)
	}
	return ""
}

func (s *Server) handleSkillGet(w http.ResponseWriter, r *http.Request, sess store.Session) {
	dir, name, ok := s.skillDir(w, r, sess)
	if !ok {
		return
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		writeErr(w, http.StatusNotFound, "技能不存在")
		return
	}
	det := skillDetail{skillInfo: s.describeSkill(dir, name), Entries: []skillEntry{}}
	if r.URL.Query().Get("scope") == "template" {
		det.Source = "template"
	} else {
		det.Source = s.skillOrigin(sess, name)
	}

	raw, err := os.ReadFile(filepath.Join(dir, skillManifest))
	switch {
	case os.IsNotExist(err):
		det.Content = ""
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	default:
		if len(raw) > skillMaxRead {
			raw, det.Truncated = raw[:skillMaxRead], true
		}
		det.Content = string(raw)
	}

	// 整棵树一次给全：技能是几个到几十个文件的量级，逐层懒加载不值当，
	// 前端拿到扁平清单自己拼树即可。WalkDir 的顺序保证父在子前。
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		if len(det.Entries) >= skillMaxEntries {
			det.More = true
			return fs.SkipAll
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		e := skillEntry{Path: filepath.ToSlash(rel), Dir: d.IsDir(), Link: d.Type()&fs.ModeSymlink != 0}
		if !e.Dir && !e.Link && !d.Type().IsRegular() {
			return nil // 设备、管道之类：技能目录里不该有，列出来也没用
		}
		if fi, ierr := d.Info(); ierr == nil { // 链接看的是链接自身，WalkDir 不跟随
			e.Size, e.MTime = fi.Size(), fi.ModTime()
			e.Exec = !e.Dir && fi.Mode().Perm()&0o111 != 0
		}
		det.Entries = append(det.Entries, e)
		return nil
	})
	writeJSON(w, http.StatusOK, det)
}

// handleSkillFile 读技能目录里的任意一个文件——文件树点开 scripts/、references/
// 里的东西走这里。默认回 JSON（文本超限截断，二进制只报大小），?raw=1 直出原始
// 字节，给图片预览和下载用。
func (s *Server) handleSkillFile(w http.ResponseWriter, r *http.Request, sess store.Session) {
	dir, _, ok := s.skillDir(w, r, sess)
	if !ok {
		return
	}
	// resolveFileEntry 逐段 Lstat 并拒绝符号链接：技能目录在会话 home 里，容器
	// 内随手就能造一个指向 /etc 的链接，跟着走就把宿主机文件读出来了。
	p, err := resolveFileEntry(dir, r.URL.Query().Get("path"))
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	info, err := os.Lstat(p)
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	if !info.Mode().IsRegular() {
		writeErr(w, http.StatusBadRequest, "不是普通文件")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()

	if r.URL.Query().Get("raw") == "1" {
		if r.URL.Query().Get("dl") == "1" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(p)))
		}
		http.ServeContent(w, r, filepath.Base(p), info.ModTime(), f)
		return
	}

	buf := make([]byte, skillMaxRead+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := skillFile{
		Path:  filepath.ToSlash(filepath.Clean(r.URL.Query().Get("path"))),
		Size:  info.Size(),
		Mode:  info.Mode().String(),
		Exec:  info.Mode().Perm()&0o111 != 0,
		MTime: info.ModTime(),
	}
	body := buf[:n]
	if n > skillMaxRead {
		body, out.Truncated = trimPartialRune(body[:skillMaxRead]), true
	}
	if bytes.IndexByte(body, 0) >= 0 || !utf8.Valid(body) {
		out.Binary = true
	} else {
		out.Content = string(body)
	}
	writeJSON(w, http.StatusOK, out)
}

// trimPartialRune 砍掉截断处残留的半个多字节字符，否则一个正常的中文文件会因为
// 结尾非法 UTF-8 被判成二进制。
func trimPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax-1 && len(b) > 0; i++ {
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size > 1 {
			break // size > 1 是文件里真有一个 U+FFFD，不是截断残渣
		}
		b = b[:len(b)-1]
	}
	return b
}

// skillDir 解析 {name} 路径参数并拼出技能目录，顺带把范围和名字校验掉。
func (s *Server) skillDir(w http.ResponseWriter, r *http.Request, sess store.Session) (dir, name string, ok bool) {
	root, err := s.skillsRoot(r.URL.Query().Get("scope"), sess)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid scope")
		return "", "", false
	}
	name = r.PathValue("name")
	if !skillNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "技能名无效")
		return "", "", false
	}
	return filepath.Join(root, name), name, true
}

func (s *Server) handleSkillDelete(w http.ResponseWriter, r *http.Request, sess store.Session) {
	dir, _, ok := s.skillDir(w, r, sess)
	if !ok {
		return
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		writeErr(w, http.StatusNotFound, "技能不存在")
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSkillCopy 在两个范围之间复制：会话里调好的技能推给自己的所有会话
// （session → template），或把模板技能立刻装进正在跑的会话（template →
// session，省得为了让模板生效重启一次）。
func (s *Server) handleSkillCopy(w http.ResponseWriter, r *http.Request, sess store.Session) {
	var req struct {
		To string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	src, name, ok := s.skillDir(w, r, sess)
	if !ok {
		return
	}
	dstRoot, err := s.skillsRoot(req.To, sess)
	if err != nil || req.To == "" {
		writeErr(w, http.StatusBadRequest, "invalid target scope")
		return
	}
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		writeErr(w, http.StatusNotFound, "技能不存在")
		return
	}
	// 复制到自己身上会先删掉源目录再无从拷起，直接拦下。
	if src == filepath.Join(dstRoot, name) {
		writeErr(w, http.StatusBadRequest, "源和目标是同一个范围")
		return
	}
	if err := ensureSkillsRoot(dstRoot); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := replaceSkillDir(src, filepath.Join(dstRoot, name)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

// handleSkillInstall 装一个技能：.zip/.tar.gz 是技能目录的打包，.md 是单文件
// 技能（写成 <名字>/SKILL.md）。同名技能整目录替换。
func (s *Server) handleSkillInstall(w http.ResponseWriter, r *http.Request, sess store.Session) {
	root, err := s.skillsRoot(r.URL.Query().Get("scope"), sess)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid scope")
		return
	}
	maxBytes := s.cfg.GetMaxUploadMB() << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "上传过大或格式不对: "+err.Error())
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer file.Close()

	// 落在会话目录下做临时区，与目标同一文件系统，最后一步 rename 才是原子的。
	stage, err := os.MkdirTemp(s.sessionDir(sess), ".stage-skill-*")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(stage)

	name := strings.TrimSpace(r.FormValue("name"))
	var srcDir string
	if archivex.IsArchiveName(hdr.Filename) {
		srcDir, name, err = stageSkillArchive(stage, hdr.Filename, file, name, maxBytes)
	} else {
		srcDir, name, err = stageSkillFile(stage, hdr.Filename, file, name, maxBytes)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !skillNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "技能名无效（只能用字母、数字、. _ -，且不超过 64 字符）")
		return
	}
	if err := ensureSkillsRoot(root); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := replaceSkillDir(srcDir, filepath.Join(root, name)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

// stageSkillArchive 解开压缩包并找出技能目录：包根就有 SKILL.md 时取包根，
// 否则取唯一的顶层目录（技能包通常带一层同名目录）。
func stageSkillArchive(stage, filename string, src io.Reader, name string, maxBytes int64) (string, string, error) {
	tmp, err := os.CreateTemp(stage, "archive-*"+filepath.Ext(filename))
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, io.LimitReader(src, maxBytes)); err != nil {
		tmp.Close()
		return "", "", err
	}
	tmp.Close()

	out := filepath.Join(stage, "unpacked")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", "", err
	}
	if _, err := archivex.Extract(tmp.Name(), filename, out, maxBytes*archivex.ExtractLimitMultiplier); err != nil {
		return "", "", err
	}

	if _, err := os.Stat(filepath.Join(out, skillManifest)); err == nil {
		if name == "" {
			name = archiveBaseName(filename)
		}
		return out, name, nil
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		return "", "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 1 {
		inner := filepath.Join(out, dirs[0])
		if _, err := os.Stat(filepath.Join(inner, skillManifest)); err == nil {
			if name == "" {
				name = dirs[0]
			}
			return inner, name, nil
		}
	}
	return "", "", fmt.Errorf("压缩包里没找到 %s（技能包应当是技能目录本身，或只含一层同名目录）", skillManifest)
}

// stageSkillFile 处理单文件技能：只有 SKILL.md 一个文件的技能直接传 .md 即可。
func stageSkillFile(stage, filename string, src io.Reader, name string, maxBytes int64) (string, string, error) {
	base := filepath.Base(filepath.Clean(filename))
	if !strings.EqualFold(filepath.Ext(base), ".md") {
		return "", "", fmt.Errorf("只支持 .md（单文件技能）或 .zip/.tar.gz（技能目录打包）")
	}
	if name == "" {
		name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	dir := filepath.Join(stage, "single")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	out, err := os.Create(filepath.Join(dir, skillManifest))
	if err != nil {
		return "", "", err
	}
	_, err = io.Copy(out, io.LimitReader(src, maxBytes))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return dir, name, err
}

func archiveBaseName(filename string) string {
	base := filepath.Base(filepath.Clean(filename))
	for _, ext := range []string{".tar.gz", ".tgz", ".tar", ".zip"} {
		if strings.HasSuffix(strings.ToLower(base), ext) {
			return base[:len(base)-len(ext)]
		}
	}
	return base
}

// ensureSkillsRoot 建出 <base>/.claude/skills。两级都可能是第一次创建，都要
// 归容器用户：服务端跑在 root 下，留个 root 属主的 ~/.claude 会让会话里的 CLI
// 写不进自己的配置目录。
//
// chown 与 files.go 一样是 best-effort：只有 root 才改得动属主，而非 root 环境
// （CI、开发机）本来就没有容器要伺候，为这个把整个操作判失败没有意义。
func ensureSkillsRoot(root string) error {
	for _, dir := range []string{filepath.Dir(root), root} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		_ = os.Chown(dir, dockerx.AgentUID, dockerx.AgentGID)
	}
	return nil
}

// replaceSkillDir 把 src 目录整体搬到 dst（先删掉旧的同名技能）。复制而不是
// rename：src 可能在别的范围里且必须保留（复制场景）。文件时间统一戳成当下，
// 这样装进模板的技能一定比各会话里的旧副本新，下次启动才会推下去。
func replaceSkillDir(src, dst string) error {
	if src == dst {
		return nil // 调用方本该拦住；这里兜底，别把源目录先删了
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	now := time.Now()
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			_ = os.Chown(target, dockerx.AgentUID, dockerx.AgentGID) // best-effort，见 ensureSkillsRoot
			return nil
		case d.Type().IsRegular():
			if err := copyRegular(p, target); err != nil {
				return err
			}
			return os.Chtimes(target, now, now)
		}
		return nil // 符号链接等不搬运：技能就是文本和脚本
	})
}

func copyRegular(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, fi.Mode().Perm()); err != nil {
		return err
	}
	_ = os.Chown(dst, dockerx.AgentUID, dockerx.AgentGID) // best-effort，见 ensureSkillsRoot
	return nil
}
