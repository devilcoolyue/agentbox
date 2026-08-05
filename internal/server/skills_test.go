package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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
