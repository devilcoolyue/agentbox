package server

import (
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agentbox/internal/dockerx"
	"agentbox/internal/store"
)

// 粘贴图片存到用户共享目录的 .images 下、其他上传附件存 .file 下
// （容器内 /shared/.images/、/shared/.file/），所有会话可见，
// 由 janitor 定期清理过期文件。
const (
	imagesSubdir = ".images"
	filesSubdir  = ".file"
	imageTTL     = 48 * time.Hour
	imageMaxMB   = 20
)

var attachExtRe = regexp.MustCompile(`^\.[a-z0-9]{1,10}$`)

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".svg": true, ".bmp": true, ".ico": true,
}

// handleImageUpload stores a pasted image or chat attachment and returns its
// container path.
func (s *Server) handleImageUpload(w http.ResponseWriter, r *http.Request, sess store.Session) {
	r.Body = http.MaxBytesReader(w, r.Body, imageMaxMB<<20)
	if err := r.ParseMultipartForm(imageMaxMB << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "image too large (max 20MB) or malformed")
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if !attachExtRe.MatchString(ext) {
		ext = ".bin"
	}
	subdir := filesSubdir
	if imageExts[ext] {
		subdir = imagesSubdir
	}
	shared, err := s.ensureSharedDir(sess.User)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dir := filepath.Join(shared, subdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Chown(dir, dockerx.AgentUID, dockerx.AgentGID)

	name := time.Now().Format("20060102-150405") + "-" + store.NewID()[:6] + ext
	dst := filepath.Join(dir, name)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, err = io.Copy(out, file)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Chown(dst, dockerx.AgentUID, dockerx.AgentGID)
	writeJSON(w, http.StatusOK, map[string]string{
		"name": name,
		"orig": filepath.Base(hdr.Filename),
		"path": dockerx.SharedMount + "/" + subdir + "/" + name,
	})
}

// imageJanitor periodically deletes pasted images older than imageTTL.
func (s *Server) imageJanitor() {
	for {
		s.cleanExpiredImages()
		time.Sleep(time.Hour)
	}
}

func (s *Server) cleanExpiredImages() {
	var dirs []string
	for _, sub := range []string{imagesSubdir, filesSubdir} {
		found, err := filepath.Glob(filepath.Join(s.cfg.DataDir, "users", "*", "shared", sub))
		if err != nil {
			continue
		}
		dirs = append(dirs, found...)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if time.Since(info.ModTime()) > imageTTL {
				if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
					log.Printf("image janitor: %v", err)
				}
			}
		}
	}
}
