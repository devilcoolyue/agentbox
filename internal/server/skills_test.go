package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/store"
)

// installSkill posts one multipart upload to the skills endpoint.
func installSkill(t *testing.T, s *Server, sess store.Session, scope, filename, name string, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if name != "" {
		_ = mw.WriteField("name", name)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/skills?scope="+scope, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	s.handleSkillInstall(w, req, sess)
	return w
}

func listSkills(t *testing.T, s *Server, sess store.Session, scope string) []skillInfo {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/skills?scope="+scope, nil)
	w := httptest.NewRecorder()
	s.handleSkillList(w, req, sess)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Skills []skillInfo `json:"skills"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Skills
}

const demoSkill = "---\nname: deploy\ndescription: 一键发布\n---\n\n步骤……\n"

func TestSkillInstallMarkdownAndList(t *testing.T) {
	s, sess := newTestServer(t)

	if w := installSkill(t, s, sess, "session", "deploy.md", "", []byte(demoSkill)); w.Code != http.StatusOK {
		t.Fatalf("install status = %d: %s", w.Code, w.Body.String())
	}
	got := listSkills(t, s, sess, "session")
	if len(got) != 1 {
		t.Fatalf("skills = %+v, want 1", got)
	}
	if got[0].Name != "deploy" || got[0].Description != "一键发布" {
		t.Fatalf("entry = %+v", got[0])
	}
	if got[0].Source != "session" {
		t.Fatalf("source = %q, want session", got[0].Source)
	}
	// The file must land where Claude Code looks for it.
	if _, err := os.Stat(filepath.Join(s.homeDir(sess), ".claude", "skills", "deploy", "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md not written: %v", err)
	}
}

func TestSkillInstallZipWithWrapperDir(t *testing.T) {
	s, sess := newTestServer(t)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"review/SKILL.md":           demoSkill,
		"review/refs/checklist.md":  "- 看看测试\n",
		"review/scripts/collect.sh": "#!/bin/sh\n",
	} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if w := installSkill(t, s, sess, "session", "review.zip", "", buf.Bytes()); w.Code != http.StatusOK {
		t.Fatalf("install status = %d: %s", w.Code, w.Body.String())
	}
	got := listSkills(t, s, sess, "session")
	if len(got) != 1 || got[0].Name != "review" {
		t.Fatalf("skills = %+v, want one named review", got)
	}
	if got[0].Files != 3 {
		t.Fatalf("files = %d, want 3", got[0].Files)
	}
	for _, rel := range []string{"refs/checklist.md", "scripts/collect.sh"} {
		p := filepath.Join(s.homeDir(sess), ".claude", "skills", "review", filepath.FromSlash(rel))
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s missing: %v", rel, err)
		}
	}
}

func TestSkillNameRejectsTraversal(t *testing.T) {
	s, sess := newTestServer(t)

	w := installSkill(t, s, sess, "session", "x.md", "../../escape", []byte(demoSkill))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if _, err := os.Stat(filepath.Join(s.sessionDir(sess), "escape")); !os.IsNotExist(err) {
		t.Fatal("traversal wrote outside the skills root")
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/skills/..", nil)
	req.SetPathValue("name", "..")
	dw := httptest.NewRecorder()
	s.handleSkillDelete(dw, req, sess)
	if dw.Code != http.StatusBadRequest {
		t.Fatalf("delete status = %d, want 400", dw.Code)
	}
}

// A skill copied into the user template must show up as template-sourced in the
// session listing — that's what tells the user it will follow them to every
// new session.
func TestSkillCopyToTemplateAndOrigin(t *testing.T) {
	s, sess := newTestServer(t)
	if w := installSkill(t, s, sess, "session", "deploy.md", "", []byte(demoSkill)); w.Code != http.StatusOK {
		t.Fatalf("install status = %d: %s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/skills/deploy/copy?scope=session",
		strings.NewReader(`{"to":"template"}`))
	req.SetPathValue("name", "deploy")
	w := httptest.NewRecorder()
	s.handleSkillCopy(w, req, sess)
	if w.Code != http.StatusOK {
		t.Fatalf("copy status = %d: %s", w.Code, w.Body.String())
	}

	tmpl := listSkills(t, s, sess, "template")
	if len(tmpl) != 1 || tmpl[0].Name != "deploy" || tmpl[0].Source != "template" {
		t.Fatalf("template listing = %+v", tmpl)
	}
	if got := listSkills(t, s, sess, "session"); len(got) != 1 || got[0].Source != "template" {
		t.Fatalf("session listing = %+v, want source=template", got)
	}

	// Deleting the session copy must leave the template one alone.
	dreq := httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/skills/deploy?scope=session", nil)
	dreq.SetPathValue("name", "deploy")
	dw := httptest.NewRecorder()
	s.handleSkillDelete(dw, dreq, sess)
	if dw.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", dw.Code, dw.Body.String())
	}
	if got := listSkills(t, s, sess, "session"); len(got) != 0 {
		t.Fatalf("session listing after delete = %+v", got)
	}
	if got := listSkills(t, s, sess, "template"); len(got) != 1 {
		t.Fatalf("template listing after session delete = %+v", got)
	}
}

// Copying a skill onto itself must be refused, not silently delete it.
func TestSkillCopySameScopeRefused(t *testing.T) {
	s, sess := newTestServer(t)
	if w := installSkill(t, s, sess, "session", "deploy.md", "", []byte(demoSkill)); w.Code != http.StatusOK {
		t.Fatalf("install status = %d: %s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/skills/deploy/copy?scope=session",
		strings.NewReader(`{"to":"session"}`))
	req.SetPathValue("name", "deploy")
	w := httptest.NewRecorder()
	s.handleSkillCopy(w, req, sess)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if got := listSkills(t, s, sess, "session"); len(got) != 1 {
		t.Fatalf("skill lost to a same-scope copy: %+v", got)
	}
}

func TestSkillDetailReadsManifest(t *testing.T) {
	s, sess := newTestServer(t)
	if w := installSkill(t, s, sess, "session", "deploy.md", "", []byte(demoSkill)); w.Code != http.StatusOK {
		t.Fatalf("install status = %d: %s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/skills/deploy", nil)
	req.SetPathValue("name", "deploy")
	w := httptest.NewRecorder()
	s.handleSkillGet(w, req, sess)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var det skillDetail
	if err := json.Unmarshal(w.Body.Bytes(), &det); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(det.Content, "步骤") || det.Description != "一键发布" {
		t.Fatalf("detail = %+v", det)
	}
}

// 技能页左侧的文件树吃的就是 detail 里的 entries：子目录和文件都要在，
// 而且父目录必须排在自己的子项前面，前端才拼得出树。
func TestSkillDetailListsTree(t *testing.T) {
	s, sess := newTestServer(t)
	root := filepath.Join(s.homeDir(sess), ".claude", "skills", "review")
	for _, dir := range []string{"scripts", "references", "assets"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel string, mode os.FileMode, body string) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("SKILL.md", 0o644, demoSkill)
	write("scripts/collect.sh", 0o755, "#!/bin/sh\necho hi\n")
	write("references/checklist.md", 0o644, "- 看看测试\n")
	write("assets/icon.png", 0o644, "\x89PNG\r\n\x1a\n\x00\x00")

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/skills/review", nil)
	req.SetPathValue("name", "review")
	w := httptest.NewRecorder()
	s.handleSkillGet(w, req, sess)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var det skillDetail
	if err := json.Unmarshal(w.Body.Bytes(), &det); err != nil {
		t.Fatal(err)
	}
	at := map[string]int{}
	for i, e := range det.Entries {
		at[e.Path] = i
	}
	for _, want := range []string{"SKILL.md", "scripts", "scripts/collect.sh", "references/checklist.md", "assets/icon.png"} {
		if _, ok := at[want]; !ok {
			t.Fatalf("entries missing %q: %+v", want, det.Entries)
		}
	}
	if at["scripts"] > at["scripts/collect.sh"] {
		t.Fatal("parent directory must be listed before its children")
	}
	if !det.Entries[at["scripts"]].Dir {
		t.Fatal("scripts not marked as a directory")
	}
	if !det.Entries[at["scripts/collect.sh"]].Exec {
		t.Fatal("executable bit not reported for scripts/collect.sh")
	}
}

func TestSkillFileReadsAndRejectsEscape(t *testing.T) {
	s, sess := newTestServer(t)
	root := filepath.Join(s.homeDir(sess), ".claude", "skills", "review")
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "collect.sh"), []byte("#!/bin/sh\necho 你好\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "logo.bin"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			"/api/sessions/s1/skills/review/file?path="+url.QueryEscape(path), nil)
		req.SetPathValue("name", "review")
		w := httptest.NewRecorder()
		s.handleSkillFile(w, req, sess)
		return w
	}

	w := get("scripts/collect.sh")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var f skillFile
	if err := json.Unmarshal(w.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Content, "echo 你好") || f.Binary || !f.Exec {
		t.Fatalf("file = %+v", f)
	}

	// 二进制只报大小，不塞内容——前端据此显示「下载」而不是乱码。
	w = get("logo.bin")
	if w.Code != http.StatusOK {
		t.Fatalf("binary status = %d: %s", w.Code, w.Body.String())
	}
	var bin skillFile
	if err := json.Unmarshal(w.Body.Bytes(), &bin); err != nil {
		t.Fatal(err)
	}
	if !bin.Binary || bin.Content != "" {
		t.Fatalf("binary file = %+v", bin)
	}

	if w := get("../../../etc/passwd"); w.Code != http.StatusBadRequest {
		t.Fatalf("traversal status = %d, want 400", w.Code)
	}

	// 符号链接同样出不去：容器里造一个指向宿主机文件的链接也读不到。
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "leak")); err != nil {
		t.Fatal(err)
	}
	if w := get("leak"); w.Code != http.StatusBadRequest {
		t.Fatalf("symlink status = %d, want 400", w.Code)
	}
}
