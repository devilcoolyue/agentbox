// 官方技能市场：浏览 anthropics/claude-plugins-official 的插件目录，并把其中
// 的技能装进会话或用户模板。
//
// 市场的单位是「插件」，插件里可能是斜杠命令、MCP 服务器、agent，也可能是
// 技能——这里只取技能（skills/<名字>/SKILL.md），取不到就明确报错，让用户去
// 终端用 claude plugin install 装完整插件。目录本身是一个 git 仓库，浅克隆到
// data/marketplace/repo 后既是目录数据源，也直接提供了 53 个一方插件的内容。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentbox/internal/store"
)

const (
	marketRepoURL = "https://github.com/anthropics/claude-plugins-official"
	// 目录更新不频繁，半天拉一次足够；用户点「刷新目录」可以强制。
	marketTTL          = 12 * time.Hour
	marketFetchTimeout = 120 * time.Second
	marketMaxSkills    = 30 // 单个插件最多装多少个技能，防目录异常撑爆
)

func (s *Server) marketDir() string  { return filepath.Join(s.cfg.DataDir, "marketplace") }
func (s *Server) marketRepo() string { return filepath.Join(s.marketDir(), "repo") }

// --- 目录数据 ---

type marketEntry struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name,omitempty"`
	Description string   `json:"description"`
	Author      string   `json:"author,omitempty"`
	Category    string   `json:"category,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	// Skills 是目录里显式声明的技能数；0 表示没声明——多数插件如此，
	// 到底有没有技能要拉下来才知道。
	Skills int `json:"skills,omitempty"`
}

// rawEntry 是 marketplace.json 里的一条，字段比对外暴露的多（source 用于安装）。
type rawEntry struct {
	Name        string          `json:"name"`
	DisplayName string          `json:"displayName"`
	Description string          `json:"description"`
	Author      json.RawMessage `json:"author"` // 可能是 {"name":…} 也可能是字符串
	Category    string          `json:"category"`
	Homepage    string          `json:"homepage"`
	Keywords    []string        `json:"keywords"`
	Tags        []string        `json:"tags"`
	Skills      []string        `json:"skills"`
	Source      json.RawMessage `json:"source"`
}

func (e rawEntry) authorName() string {
	if len(e.Author) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(e.Author, &asString) == nil {
		return asString
	}
	var obj struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(e.Author, &obj)
	return obj.Name
}

func (e rawEntry) public() marketEntry {
	kw := e.Keywords
	if len(kw) == 0 {
		kw = e.Tags
	}
	return marketEntry{
		Name:        e.Name,
		DisplayName: e.DisplayName,
		Description: e.Description,
		Author:      e.authorName(),
		Category:    e.Category,
		Homepage:    e.Homepage,
		Keywords:    kw,
		Skills:      len(e.Skills),
	}
}

// pluginSource 归一化四种 source 写法：仓库内相对路径、git-subdir、url、github。
// 除第一种外都要另外克隆一个仓库。
type pluginSource struct {
	Local bool   // true = 就在市场仓库里，Path 是仓库内相对路径
	URL   string // 待克隆的 git 地址
	Ref   string // 分支或标签；40 位 sha 走 fetch 而不是 --branch
	Path  string // 仓库内的插件子目录
}

func parsePluginSource(raw json.RawMessage) (pluginSource, error) {
	var rel string
	if err := json.Unmarshal(raw, &rel); err == nil {
		clean := path.Clean(strings.TrimPrefix(rel, "./"))
		if clean == "" || path.IsAbs(clean) || !filepath.IsLocal(filepath.FromSlash(clean)) {
			return pluginSource{}, fmt.Errorf("目录里的插件路径不合法: %q", rel)
		}
		return pluginSource{Local: true, Path: clean}, nil
	}
	var obj struct {
		Source string `json:"source"`
		URL    string `json:"url"`
		Repo   string `json:"repo"`
		Path   string `json:"path"`
		Ref    string `json:"ref"`
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return pluginSource{}, fmt.Errorf("无法解析插件来源: %w", err)
	}
	out := pluginSource{URL: obj.URL, Ref: obj.Ref}
	if out.Ref == "" {
		out.Ref = obj.Commit
	}
	if obj.Repo != "" && out.URL == "" {
		out.URL = "https://github.com/" + obj.Repo
	}
	if obj.Path != "" {
		clean := path.Clean(strings.TrimPrefix(obj.Path, "./"))
		if path.IsAbs(clean) || !filepath.IsLocal(filepath.FromSlash(clean)) {
			return pluginSource{}, fmt.Errorf("插件子路径不合法: %q", obj.Path)
		}
		out.Path = clean
	}
	if !strings.HasPrefix(out.URL, "https://") {
		// 目录里全是 https 的 GitHub 地址；别的协议（ssh/file/git）一律不碰。
		return pluginSource{}, fmt.Errorf("不支持的插件来源: %q", out.URL)
	}
	return out, nil
}

// ensureMarketRepo 保证市场仓库在本地且不太旧。过期就整个重克隆——浅克隆才
// 10MB 一秒多，比增量更新少一堆状态处理。拉不动时若已有旧副本就接着用。
func (s *Server) ensureMarketRepo(ctx context.Context, force bool) (string, error) {
	repo := s.marketRepo()
	stamp := filepath.Join(s.marketDir(), "fetched-at")
	fresh := false
	if fi, err := os.Stat(stamp); err == nil {
		fresh = time.Since(fi.ModTime()) < marketTTL
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude-plugin", "marketplace.json")); err == nil && fresh && !force {
		return repo, nil
	}
	if err := os.MkdirAll(s.marketDir(), 0o755); err != nil {
		return "", err
	}

	tmp := repo + ".new"
	_ = os.RemoveAll(tmp)
	if err := gitClone(ctx, marketRepoURL, "", tmp); err != nil {
		_ = os.RemoveAll(tmp)
		if _, serr := os.Stat(filepath.Join(repo, ".claude-plugin", "marketplace.json")); serr == nil {
			return repo, nil // 拉不动就用旧的，别让浏览功能整个瘫掉
		}
		return "", fmt.Errorf("拉取官方市场目录失败: %w", err)
	}
	old := repo + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(repo); err == nil {
		if err := os.Rename(repo, old); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
	}
	if err := os.Rename(tmp, repo); err != nil {
		_ = os.Rename(old, repo)
		return "", err
	}
	_ = os.RemoveAll(old)
	_ = os.WriteFile(stamp, []byte(time.Now().Format(time.RFC3339)), 0o644)
	return repo, nil
}

func (s *Server) readMarket(ctx context.Context, force bool) ([]rawEntry, time.Time, error) {
	repo, err := s.ensureMarketRepo(ctx, force)
	if err != nil {
		return nil, time.Time{}, err
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return nil, time.Time{}, err
	}
	var doc struct {
		Plugins []rawEntry `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, time.Time{}, fmt.Errorf("市场目录解析失败: %w", err)
	}
	var at time.Time
	if fi, err := os.Stat(filepath.Join(s.marketDir(), "fetched-at")); err == nil {
		at = fi.ModTime()
	}
	return doc.Plugins, at, nil
}

// handleMarketList 返回整份目录（不到 200KB），搜索和分类筛选交给前端做，
// 免得每敲一个字就打一次服务端。
func (s *Server) handleMarketList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), marketFetchTimeout)
	defer cancel()

	s.marketMu.Lock()
	entries, at, err := s.readMarket(ctx, r.URL.Query().Get("refresh") == "1")
	s.marketMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]marketEntry, 0, len(entries))
	cats := map[string]bool{}
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		out = append(out, e.public())
		if e.Category != "" {
			cats[e.Category] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	categories := make([]string, 0, len(cats))
	for c := range cats {
		categories = append(categories, c)
	}
	sort.Strings(categories)
	writeJSON(w, http.StatusOK, map[string]any{
		"plugins":    out,
		"categories": categories,
		"updated_at": at,
		"source":     marketRepoURL,
	})
}

// --- 安装 ---

func (s *Server) handleSkillMarketInstall(w http.ResponseWriter, r *http.Request, sess store.Session) {
	root, err := s.skillsRoot(r.URL.Query().Get("scope"), sess)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid scope")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), marketFetchTimeout)
	defer cancel()

	// git 抓取重且并发无益，整条安装链路串起来跑。
	s.marketMu.Lock()
	defer s.marketMu.Unlock()

	entries, _, err := s.readMarket(ctx, false)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	var entry *rawEntry
	for i := range entries {
		if entries[i].Name == req.Name {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		writeErr(w, http.StatusNotFound, "市场里没有这个插件")
		return
	}
	src, err := parsePluginSource(entry.Source)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	repo, err := s.ensureMarketRepo(ctx, false)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	pluginDir := filepath.Join(repo, filepath.FromSlash(src.Path))
	if !src.Local {
		work, err := os.MkdirTemp(s.marketDir(), "fetch-*")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer os.RemoveAll(work)
		if err := gitClone(ctx, src.URL, src.Ref, work); err != nil {
			writeErr(w, http.StatusBadGateway, "拉取插件源失败: "+err.Error())
			return
		}
		pluginDir = filepath.Join(work, filepath.FromSlash(src.Path))
	}
	if fi, err := os.Stat(pluginDir); err != nil || !fi.IsDir() {
		writeErr(w, http.StatusBadGateway, "插件源里找不到目录 "+src.Path)
		return
	}

	dirs := discoverSkillDirs(pluginDir, entry.Skills)
	if len(dirs) == 0 {
		writeErr(w, http.StatusUnprocessableEntity,
			"「"+entry.Name+"」不含技能——它可能只提供斜杠命令或 MCP 服务器。"+
				"整包安装请在工作空间终端里跑：claude plugin install "+entry.Name+"@"+"claude-plugins-official")
		return
	}
	if err := ensureSkillsRoot(root); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	installed := []string{}
	for _, dir := range dirs {
		name := filepath.Base(dir)
		if !skillNameRe.MatchString(name) {
			continue
		}
		if err := replaceSkillDir(dir, filepath.Join(root, name)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		installed = append(installed, name)
	}
	if len(installed) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, "插件里的技能目录名不合法，装不了")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"installed": installed})
}

// discoverSkillDirs 在插件目录里找技能：优先目录里显式声明的路径，其次约定的
// skills/<名字>/，最后插件本身就是一个技能的情况。
func discoverSkillDirs(pluginDir string, declared []string) []string {
	var out []string
	add := func(dir string) {
		if len(out) >= marketMaxSkills {
			return
		}
		if fi, err := os.Stat(filepath.Join(dir, skillManifest)); err == nil && !fi.IsDir() {
			out = append(out, dir)
		}
	}
	for _, rel := range declared {
		clean := path.Clean(strings.TrimPrefix(rel, "./"))
		if clean == "" || path.IsAbs(clean) || !filepath.IsLocal(filepath.FromSlash(clean)) {
			continue
		}
		add(filepath.Join(pluginDir, filepath.FromSlash(clean)))
	}
	if len(out) > 0 {
		return out
	}
	if entries, err := os.ReadDir(filepath.Join(pluginDir, "skills")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				add(filepath.Join(pluginDir, "skills", e.Name()))
			}
		}
	}
	if len(out) == 0 {
		add(pluginDir)
	}
	return out
}

// gitClone 浅克隆到 dst。ref 是分支或标签时直接 --branch；是 40 位提交号时先
// 克隆默认分支再单独 fetch 那个提交，取不到就留在默认分支（内容会比目录里钉的
// 版本新，可接受）。
func gitClone(ctx context.Context, url, ref, dst string) error {
	args := []string{"clone", "--depth", "1", "--single-branch", "--no-tags", "-q"}
	sha := isHexSHA(ref)
	if ref != "" && !sha {
		args = append(args, "--branch", ref)
	}
	args = append(args, url, dst)
	if err := runGitPlain(ctx, args...); err != nil {
		if ref != "" && !sha {
			// 分支/标签不存在时退回默认分支，别让整次安装挂掉
			_ = os.RemoveAll(dst)
			return runGitPlain(ctx, "clone", "--depth", "1", "--single-branch", "--no-tags", "-q", url, dst)
		}
		return err
	}
	if sha {
		if err := runGitPlain(ctx, "-C", dst, "fetch", "--depth", "1", "-q", "origin", ref); err == nil {
			_ = runGitPlain(ctx, "-C", dst, "checkout", "-q", "FETCH_HEAD")
		}
	}
	return nil
}

func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// runGitPlain 跑一条不绑定工作目录的 git。GIT_TERMINAL_PROMPT=0 保证私有仓库
// 直接失败而不是挂在密码提示上把请求卡死。
func runGitPlain(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GCM_INTERACTIVE=never")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}
