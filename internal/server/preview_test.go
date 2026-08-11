package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/store"
)

// grantFor 走一遍真实的签发路径，返回 /preview/<id>/<name> 形式的直链。
func grantFor(t *testing.T, s *Server, sess store.Session, path string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/g?path="+path, nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxUser, store.User{Name: sess.User}))
	w := httptest.NewRecorder()
	s.handlePreviewGrant(w, r, sess)
	if w.Code != http.StatusOK {
		t.Fatalf("grant status = %d, body %s", w.Code, w.Body.String())
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.URL
}

func newPreviewServer(t *testing.T) (*Server, store.Session, string) {
	t.Helper()
	s, sess := newTestServer(t)
	s.previews = newPreviewStore()
	ws := s.workspaceDir(sess)
	if err := os.MkdirAll(filepath.Join(ws, "proto", "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "proto", "index.html"),
		[]byte(`<link rel="stylesheet" href="assets/app.css">`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "proto", "assets", "app.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "secret.txt"), []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	return s, sess, ws
}

func servePreview(s *Server, url string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handlePreviewServe(w, httptest.NewRequest(http.MethodGet, url, nil))
	return w
}

func TestPreviewServesFileAndRelativeAsset(t *testing.T) {
	s, sess, _ := newPreviewServer(t)
	url := grantFor(t, s, sess, "proto/index.html")
	if !strings.HasSuffix(url, "/index.html") {
		t.Fatalf("grant url = %q, want it to end in the file name", url)
	}

	w := servePreview(s, url)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "app.css") {
		t.Fatalf("document status = %d, body %q", w.Code, w.Body.String())
	}
	// 页面里的相对引用必须能顺着同一个证号解析出来，这正是证号走路径而不是
	// 查询串的原因。
	base := strings.TrimSuffix(url, "index.html")
	if w := servePreview(s, base+"assets/app.css"); w.Code != http.StatusOK || w.Body.String() != "body{}" {
		t.Fatalf("asset status = %d, body %q", w.Code, w.Body.String())
	}
}

func TestPreviewSandboxHeaders(t *testing.T) {
	s, sess, _ := newPreviewServer(t)
	w := servePreview(s, grantFor(t, s, sess, "proto/index.html"))

	csp := w.Header().Get("Content-Security-Policy")
	if !strings.HasPrefix(csp, "sandbox ") || !strings.Contains(csp, "allow-scripts") {
		t.Fatalf("CSP = %q, want a sandbox that still allows scripts", csp)
	}
	// allow-same-origin 会让原型脚本回到控制台同源，直接读走 localStorage 的令牌
	if strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("CSP = %q, must never grant same-origin to previewed HTML", csp)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestPreviewGrantIsConfinedToItsDirectory(t *testing.T) {
	s, sess, _ := newPreviewServer(t)
	base := strings.TrimSuffix(grantFor(t, s, sess, "proto/index.html"), "index.html")

	// 通行证只覆盖 proto/，工作区里的邻居文件不能顺着 .. 读出来
	if w := servePreview(s, base+"../secret.txt"); w.Code == http.StatusOK {
		t.Fatalf("escaped the grant directory: status %d, body %q", w.Code, w.Body.String())
	}
}

func TestPreviewRejectsUnknownAndExpiredGrant(t *testing.T) {
	s, sess, _ := newPreviewServer(t)
	if w := servePreview(s, "/preview/nope/index.html"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown grant status = %d, want 404", w.Code)
	}

	url := grantFor(t, s, sess, "proto/index.html")
	id := strings.Split(strings.TrimPrefix(url, previewPrefix), "/")[0]
	s.previews.mu.Lock()
	g := s.previews.grants[id]
	g.expires = time.Now().Add(-time.Minute)
	s.previews.grants[id] = g
	s.previews.mu.Unlock()

	if w := servePreview(s, url); w.Code != http.StatusNotFound {
		t.Fatalf("expired grant status = %d, want 404", w.Code)
	}
	if _, ok := s.previews.lookup(id); ok {
		t.Fatal("expired grant still resolvable")
	}
}

func TestPreviewGrantReusedPerDirectory(t *testing.T) {
	s, sess, ws := newPreviewServer(t)
	if err := os.WriteFile(filepath.Join(ws, "proto", "other.html"), []byte("<b>x</b>"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := strings.Split(strings.TrimPrefix(grantFor(t, s, sess, "proto/index.html"), previewPrefix), "/")[0]
	second := strings.Split(strings.TrimPrefix(grantFor(t, s, sess, "proto/other.html"), previewPrefix), "/")[0]
	if first != second {
		t.Fatalf("same directory issued two grants (%s, %s); repeated opens would pile up", first, second)
	}
	if n := len(s.previews.grants); n != 1 {
		t.Fatalf("live grants = %d, want 1", n)
	}
}

func TestPreviewInjectsStorageShimBeforePageScripts(t *testing.T) {
	s, sess, ws := newPreviewServer(t)
	page := `<!doctype html><html><head><title>t</title><script>var x = localStorage.getItem("k")</script></head>` +
		`<body><header>nav</header></body></html>`
	if err := os.WriteFile(filepath.Join(ws, "proto", "index.html"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	body := servePreview(s, grantFor(t, s, sess, "proto/index.html")).Body.String()

	shim := strings.Index(body, "sessionStorage")
	pageScript := strings.Index(body, `localStorage.getItem("k")`)
	if shim < 0 || pageScript < 0 {
		t.Fatalf("shim=%d pageScript=%d, both should be present:\n%s", shim, pageScript, body)
	}
	// 排在原型自己的脚本后面就没有意义了：那时它已经撞上 SecurityError 了
	if shim > pageScript {
		t.Fatal("shim injected after the page's own script")
	}
	if i := strings.Index(body, "<head>"); i < 0 || shim < i {
		t.Fatalf("shim should land just inside <head>, got head=%d shim=%d", i, shim)
	}
}

func TestShimInsertPoint(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// <header> 是 <head 的前缀，别插进它中间
		{"header first", "<header>x</header><head><title>t</title></head>", "<head>"},
		{"head with attrs", `<html><head lang="zh"><meta></head>`, `<head lang="zh">`},
		{"uppercase", "<HTML><HEAD><META></HEAD>", "<HEAD>"},
		{"no head", "<html><body>x</body></html>", "<html>"},
		{"fragment only", "<div>x</div>", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at := shimInsertPoint([]byte(c.in))
			if got := c.in[:at]; !strings.HasSuffix(strings.ToLower(got), strings.ToLower(c.want)) {
				t.Fatalf("insert point %d leaves prefix %q, want it to end with %q", at, got, c.want)
			}
		})
	}
}

func TestPreviewLeavesNonHTMLAndSourceUntouched(t *testing.T) {
	s, sess, _ := newPreviewServer(t)
	base := strings.TrimSuffix(grantFor(t, s, sess, "proto/index.html"), "index.html")

	// 子资源不是文档，注入进去只会把 CSS 弄坏
	if got := servePreview(s, base+"assets/app.css").Body.String(); got != "body{}" {
		t.Fatalf("css body = %q, want it byte-for-byte", got)
	}
	// 源码视图 / 下载走的是另一条接口，必须还是原文件
	w := httptest.NewRecorder()
	s.handleFileGet(w, httptest.NewRequest(http.MethodGet, "/f?path=proto/index.html", nil), sess)
	if strings.Contains(w.Body.String(), "sessionStorage") {
		t.Fatalf("source view leaked the preview shim:\n%s", w.Body.String())
	}
}

func TestFileGetBlocksScriptsUnlessDownloading(t *testing.T) {
	s, sess := newTestServer(t)
	ws := s.workspaceDir(sess)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("x.html", "<script>fetch('/api')</script>")
	write("logo.svg", `<svg xmlns="http://www.w3.org/2000/svg"><script/></svg>`)
	write("notes.txt", "plain")
	write("noext", "<html><script>alert(1)</script></html>")

	get := func(q string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleFileGet(w, httptest.NewRequest(http.MethodGet, "/f?path="+q, nil), sess)
		return w
	}

	// 能当文档执行脚本的类型：一律禁脚本
	for _, name := range []string{"x.html", "logo.svg", "noext"} {
		if got := get(name).Header().Get("Content-Security-Policy"); got != "sandbox" {
			t.Fatalf("%s CSP = %q, want a script-free sandbox", name, got)
		}
	}
	// SVG 还得保住图片类型，否则文件页的图片预览会瞎掉
	if ct := get("logo.svg").Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("svg Content-Type = %q, want image/svg+xml", ct)
	}
	// 其余类型不设 CSP：sandbox 会让 Chrome 的 PDF 阅读器之类退化成下载
	if got := get("notes.txt").Header().Get("Content-Security-Policy"); got != "" {
		t.Fatalf("text CSP = %q, want none", got)
	}
	if got := get("notes.txt").Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	// 下载响应不建立浏览上下文，加 CSP 只会给存盘添乱
	if got := get("x.html&dl=1").Header().Get("Content-Security-Policy"); got != "" {
		t.Fatalf("download CSP = %q, want none", got)
	}
}
