package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func attachmentUploadRequest(t *testing.T, field, name string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/images", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	return r
}

func TestChatAttachmentUploadLimits(t *testing.T) {
	s, sess := newTestServer(t)
	s.cfg.MaxUploadMB = 1
	for _, size := range []int{(1 << 20) + 1, 3 << 20} {
		for _, chunked := range []bool{false, true} {
			r := attachmentUploadRequest(t, "file", "large.pdf", bytes.Repeat([]byte("x"), size))
			if chunked {
				r.ContentLength = -1
			}
			w := httptest.NewRecorder()
			s.handleImageUpload(w, r, sess)
			if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "max 1MB") {
				t.Fatalf("size=%d chunked=%v: status=%d body=%s", size, chunked, w.Code, w.Body)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "users", sess.User, "shared")); !os.IsNotExist(err) {
		t.Fatalf("rejected upload touched shared storage: %v", err)
	}
}

func TestChatAttachmentUploadLargeFiles(t *testing.T) {
	s, sess := newTestServer(t)
	if _, err := s.ensureSharedDir(sess.User); err != nil {
		if os.IsPermission(err) {
			t.Skip("requires chown to container UID 1000; run in isolated Linux container")
		}
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mb   int64
		size int
		dir  string
	}{
		{"boundary.pdf", 1, 1 << 20, filesSubdir},
		{"large.pdf", 40, 33 << 20, filesSubdir},
		{"image.png", 1, 1024, imagesSubdir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			s.cfg.MaxUploadMB = tc.mb // A later request must see the current setting.
			data := bytes.Repeat([]byte("x"), tc.size)
			r := attachmentUploadRequest(t, "file", tc.name, data)
			w := httptest.NewRecorder()
			s.handleImageUpload(w, r, sess)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			var result struct{ Name, Orig, Path string }
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Orig != tc.name || result.Path != "/shared/"+tc.dir+"/"+result.Name {
				t.Fatalf("invalid upload response: %+v", result)
			}
			stored, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "users", sess.User, "shared", tc.dir, result.Name))
			if err != nil || !bytes.Equal(stored, data) {
				t.Fatalf("stored file differs: %v", err)
			}
			if entries, err := os.ReadDir(temp); err != nil || len(entries) != 0 {
				t.Fatalf("multipart temporary files leaked: %v %v", entries, err)
			}
		})
	}
}

func TestChatAttachmentUploadMalformedCleanup(t *testing.T) {
	s, sess := newTestServer(t)
	s.cfg.MaxUploadMB = 40
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	// Parsing succeeds and spills to disk, but the required file field is absent.
	r := attachmentUploadRequest(t, "wrong-field", "large.pdf", bytes.Repeat([]byte("x"), 33<<20))
	w := httptest.NewRecorder()
	s.handleImageUpload(w, r, sess)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if entries, err := os.ReadDir(temp); err != nil || len(entries) != 0 {
		t.Fatalf("multipart temporary files leaked on error: %v %v", entries, err)
	}
	r = httptest.NewRequest(http.MethodPost, "/images", strings.NewReader("malformed"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
	w = httptest.NewRecorder()
	s.handleImageUpload(w, r, sess)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status=%d", w.Code)
	}
}
