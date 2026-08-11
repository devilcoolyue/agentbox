package server

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentbox/internal/store"
)

// HTML 预览：文件弹窗把工作区里的 .html 丢进 iframe 直接渲染，Agent 改一版就能
// 看一版。原型自带的 <script> 会真的跑起来，所以它绝不能看见控制台的登录令牌 ——
// 预览因此不复用 ?token=，而是另发一张「预览通行证」：
//
//   - 只读、只覆盖被预览文件所在的那一个目录、30 分钟过期；
//   - 证号走 URL 路径而不是查询串，页面里 ./style.css、./img/a.png 这类相对引用
//     才能自然解析（?path= 那种查询串形式会把相对路径解析到别的地方去）；
//   - 响应带 CSP sandbox（不含 allow-same-origin），文档落在 opaque origin，
//     既读不到 localStorage 里的令牌，也带不动同源接口的凭证。
//
// 通行证本身一定会暴露给原型页面的 JS（它能读自己的 location），所以范围和时效
// 必须小：真泄露了，也只是那一个目录、只读、半小时。
const (
	previewTTL     = 30 * time.Minute
	previewMaxLive = 200 // 同时存活的通行证上限，防止无限增长
	previewPrefix  = "/preview/"
)

type previewGrant struct {
	user    string
	dir     string // 宿主机绝对路径，通行证只能读这个目录（及其子目录）
	expires time.Time
}

// previewStore 只存在内存里：通行证熬过一次重启没有任何价值，只会把窗口拉长。
type previewStore struct {
	mu     sync.Mutex
	grants map[string]previewGrant
}

func newPreviewStore() *previewStore { return &previewStore{grants: map[string]previewGrant{}} }

// issue 为 (user, dir) 发证。同一个用户反复打开同一个目录复用同一张证并续期，
// 既让 iframe 的 URL 保持稳定，也避免每次点开都堆一张新证。
func (p *previewStore) issue(user, dir string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()

	exp := time.Now().Add(previewTTL)
	for id, g := range p.grants {
		if g.user == user && g.dir == dir {
			g.expires = exp
			p.grants[id] = g
			return id, true
		}
	}
	if len(p.grants) >= previewMaxLive {
		return "", false
	}
	id := randomPreviewID()
	p.grants[id] = previewGrant{user: user, dir: dir, expires: exp}
	return id, true
}

func (p *previewStore) lookup(id string) (previewGrant, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	g, ok := p.grants[id]
	if !ok {
		return previewGrant{}, false
	}
	if time.Now().After(g.expires) {
		delete(p.grants, id)
		return previewGrant{}, false
	}
	return g, true
}

func (p *previewStore) sweepLocked() {
	now := time.Now()
	for id, g := range p.grants {
		if now.After(g.expires) {
			delete(p.grants, id)
		}
	}
}

func randomPreviewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// handlePreviewGrant 为一个文件签发预览直链，返回的 URL 交给 iframe 当 src。
func (s *Server) handlePreviewGrant(w http.ResponseWriter, r *http.Request, sess store.Session) {
	p, err := s.resolveFile(r, sess)
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
	id, ok := s.previews.issue(reqUser(r).Name, filepath.Dir(p))
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "预览链接过多，请稍后再试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url":        previewPrefix + id + "/" + url.PathEscape(filepath.Base(p)),
		"expires_in": int(previewTTL / time.Second),
	})
}

// handlePreviewServe 按通行证直出文件。刻意不挂 s.auth：子资源（css/js/图片）
// 是浏览器自己按相对路径发的请求，带不上 Authorization 头，证号在路径里才是唯一
// 能让它们跟着走的凭据。
func (s *Server) handlePreviewServe(w http.ResponseWriter, r *http.Request) {
	id, rel, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, previewPrefix), "/")
	grant, ok := s.previews.lookup(id)
	if !ok {
		http.Error(w, "预览链接已失效，请重新打开预览", http.StatusNotFound)
		return
	}
	if rel == "" {
		rel = "index.html"
	}
	p, err := resolveFileEntry(grant.dir, rel)
	if err != nil {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "读取失败", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	// sandbox 不带 allow-same-origin：脚本照跑，但文档是 opaque origin，拿不到
	// 控制台的 localStorage / 同源凭证。no-store 保证「刷新」拿到的一定是新内容。
	w.Header().Set("Content-Security-Policy",
		"sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads allow-pointer-lock")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")

	name := filepath.Base(p)
	if isHTMLName(name) && info.Size() <= previewMaxInject {
		if raw, err := io.ReadAll(f); err == nil {
			body := injectPreviewShim(raw)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)
			return
		}
		// 读失败就退回原样下发，至少别把预览整个弄丢
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "读取失败", http.StatusInternalServerError)
			return
		}
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func isHTMLName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html", ".htm":
		return true
	}
	return false
}

// opaque origin 缺的那几样，在这里补回来。sandbox 不给 allow-same-origin（给了
// 原型脚本就能读走控制台的登录令牌），代价是：
//   - localStorage / sessionStorage 一碰就抛 SecurityError，原型里一句没包
//     try-catch 的 localStorage.getItem 就能打断整段初始化，页面看着像坏了；
//   - history.pushState / replaceState 同样抛错，前端路由型原型点了就白屏。
//
// 换成内存实现后行为反而更接近真实浏览器：能存能取，关掉就丢。注入只发生在
// /preview/ 下发的 HTML 上，源码视图和下载拿到的仍然是原文件一个字节没动。
// document.cookie 不用管：opaque origin 下它是静默读空、写无效，不抛错。
const previewShim = `<script>
(function () {
  "use strict";
  var need = false;
  try { void window.localStorage; } catch (e) { need = true; }
  if (need) {
    var mem = function () {
      var map = new Map();
      var api = {
        getItem: function (k) { k = String(k); return map.has(k) ? map.get(k) : null; },
        setItem: function (k, v) { map.set(String(k), String(v)); },
        removeItem: function (k) { map.delete(String(k)); },
        clear: function () { map.clear(); },
        key: function (i) { var ks = Array.from(map.keys()); i = Number(i); return i >= 0 && i < ks.length ? ks[i] : null; }
      };
      var own = function (p) { return typeof p === "string" && Object.prototype.hasOwnProperty.call(api, p); };
      return new Proxy(api, {
        get: function (t, p) {
          if (p === "length") return map.size;
          if (own(p) || typeof p !== "string") return t[p];
          return map.has(p) ? map.get(p) : undefined;
        },
        set: function (t, p, v) {
          if (typeof p === "string" && p !== "length" && !own(p)) map.set(p, String(v));
          return true;
        },
        has: function (t, p) { return (typeof p === "string" && map.has(p)) || p in t; },
        deleteProperty: function (t, p) { if (typeof p === "string") map.delete(p); return true; },
        ownKeys: function () { return Array.from(map.keys()); },
        getOwnPropertyDescriptor: function (t, p) {
          if (typeof p === "string" && map.has(p)) {
            return { value: map.get(p), writable: true, enumerable: true, configurable: true };
          }
          return undefined;
        }
      });
    };
    ["localStorage", "sessionStorage"].forEach(function (n) {
      try { Object.defineProperty(window, n, { value: mem(), configurable: true }); } catch (e) {}
    });
  }
  ["pushState", "replaceState"].forEach(function (m) {
    var orig = window.history && window.history[m];
    if (typeof orig !== "function") return;
    try {
      window.history[m] = function () {
        try { return orig.apply(window.history, arguments); } catch (e) { /* 改不了地址栏，页面自己的渲染照常 */ }
      };
    } catch (e) {}
  });
})();
</script>`

// previewMaxInject 之外的 HTML 原样流式下发，不为了注入把大文件读进内存。
const previewMaxInject = 8 << 20

// injectPreviewShim 把 shim 插到 <head> 之后 —— 必须排在原型自己的任何脚本前面，
// 否则它已经先撞上 SecurityError 了。没有 <head> 就退到 <html> 之后，再没有就
// 放最前面（浏览器照样会把它归进隐式 head）。
func injectPreviewShim(raw []byte) []byte {
	at := shimInsertPoint(raw)
	out := make([]byte, 0, len(raw)+len(previewShim))
	out = append(out, raw[:at]...)
	out = append(out, previewShim...)
	return append(out, raw[at:]...)
}

func shimInsertPoint(raw []byte) int {
	n := min(len(raw), 65536) // <head> 不会藏在 64KB 之后
	head := strings.ToLower(string(raw[:n]))
	for _, tag := range []string{"<head", "<html"} {
		i := strings.Index(head, tag)
		// "<head" 也是 "<header" 的前缀，得确认标签名到此为止
		for i >= 0 && i+len(tag) < len(head) && !isTagNameEnd(head[i+len(tag)]) {
			next := strings.Index(head[i+1:], tag)
			if next < 0 {
				i = -1
				break
			}
			i += 1 + next
		}
		if i < 0 {
			continue
		}
		if j := strings.IndexByte(head[i:], '>'); j >= 0 {
			return i + j + 1
		}
	}
	return 0
}

func isTagNameEnd(c byte) bool {
	return c == '>' || c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '/'
}

// scriptableDoc 判断这个类型会不会被浏览器当成「能执行脚本的文档」打开。
// 只对这几类收紧，是为了别误伤 PDF —— Chrome 的内置阅读器在 sandbox 文档里会
// 退化成直接下载。
func scriptableDoc(ct string) bool {
	base, _, _ := strings.Cut(ct, ";")
	switch strings.ToLower(strings.TrimSpace(base)) {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "text/xml", "application/xml":
		return true
	}
	return false
}

// serveRawInline 直出文件原始字节，并按内容类型收紧安全头。
//
// Agent 生成或用户上传的 HTML/SVG 一旦被当作文档打开，就会在控制台同源里执行
// 脚本，能直接读走 localStorage 里的登录令牌 —— 这类响应一律挂不带任何 allow-*
// 的 sandbox，脚本不执行。真要跑脚本的预览走 /preview/ 那条独立限权的路径。
// 带 Content-Disposition 的下载不建立浏览上下文，不走这里。
func serveRawInline(w http.ResponseWriter, r *http.Request, f *os.File, info os.FileInfo) {
	name := info.Name()
	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		// 没有扩展名时按 net/http 的同款嗅探定类型：靠 ServeContent 自己嗅的话，
		// 类型定下来时头已经发出去了，来不及再补 CSP。
		var buf [512]byte
		n, _ := io.ReadFull(f, buf[:])
		ct = http.DetectContentType(buf[:n])
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if scriptableDoc(ct) {
		w.Header().Set("Content-Security-Policy", "sandbox")
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}
