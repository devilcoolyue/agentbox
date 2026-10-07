package server

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agentbox/internal/dockerx"
	"agentbox/internal/safefs"
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
	root, err := s.openDataDir(shared)
	if err != nil {
		writeFileOpErr(w, err)
		return
	}
	defer root.Close()
	if err := root.MkdirAll(subdir, 0o755); err != nil {
		writeFileOpErr(w, err)
		return
	}
	_ = root.Chown(subdir, dockerx.AgentUID, dockerx.AgentGID)
	name := time.Now().Format("20060102-150405") + "-" + store.NewID()[:6] + ext
	if _, err := root.WriteAtomic(filepath.Join(subdir, name), file, safefs.WriteOptions{Mode: 0o644, Chown: true, UID: dockerx.AgentUID, GID: dockerx.AgentGID, BestEffortChown: true, MaxBytes: imageMaxMB << 20}); err != nil {
		writeFileOpErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"name": name,
		"orig": filepath.Base(hdr.Filename),
		"path": dockerx.SharedMount + "/" + subdir + "/" + name,
	})
}

// imageJanitor periodically deletes pasted images older than imageTTL.
func (s *Server) imageJanitor() {
	for s.workContext().Err() == nil {
		s.cleanExpiredImages()
		if !waitInterval(s.workContext(), time.Hour) {
			return
		}
	}
}

// attachRefRe matches the stored container path of an attachment inside a chat
// transcript, e.g. "[图片#1 /shared/.images/20260726-101500-ab12cd.png]".
var attachRefRe = regexp.MustCompile(`/shared/\.(?:images|file)/([A-Za-z0-9._-]+)`)

// referencedAttachments returns the attachment file names a user's chat
// transcripts still point at. Those must survive the TTL sweep: an expired
// image turns every past message that showed it into a broken placeholder,
// which contradicts the promise that conversations are kept.
func (s *Server) referencedAttachments(userDir string) map[string]bool {
	refs := map[string]bool{}
	transcripts, err := filepath.Glob(filepath.Join(userDir, "sessions", "*", "chats", "*.jsonl"))
	if err != nil {
		return refs
	}
	// The legacy single-file transcript may still be around next to the session.
	if legacy, err := filepath.Glob(filepath.Join(userDir, "sessions", "*", "chat.jsonl")); err == nil {
		transcripts = append(transcripts, legacy...)
	}
	for _, path := range transcripts {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, m := range attachRefRe.FindAllSubmatch(raw, -1) {
			refs[string(m[1])] = true
		}
	}
	return refs
}

func (s *Server) cleanExpiredImages() {
	s.attachmentMu.Lock()
	defer s.attachmentMu.Unlock()
	userDirs, err := filepath.Glob(filepath.Join(s.cfg.DataDir, "users", "*"))
	if err != nil {
		return
	}
	for _, userDir := range userDirs {
		var expired []string
		for _, sub := range []string{imagesSubdir, filesSubdir} {
			dir := filepath.Join(userDir, "shared", sub)
			area, err := s.openDataDir(dir)
			if err != nil {
				continue
			}
			entries, err := area.ReadDir(".")
			area.Close()
			if err != nil {
				continue
			}
			for _, e := range entries {
				info, err := e.Info()
				if err != nil || !info.Mode().IsRegular() {
					continue
				}
				if time.Since(info.ModTime()) > imageTTL {
					expired = append(expired, filepath.Join(dir, e.Name()))
				}
			}
		}
		if len(expired) == 0 {
			continue // 没有过期候选就不必读转录，省掉绝大多数扫描
		}
		refs := s.referencedAttachments(userDir)
		err = s.store.VisitChatAttachmentInputs(filepath.Base(userDir), func(input store.ChatRequestInput) {
			for _, path := range input.Attachments {
				refs[filepath.Base(path)] = true
			}
			for _, match := range attachRefRe.FindAllStringSubmatch(input.Text, -1) {
				refs[match[1]] = true
			}
		})
		if err != nil {
			continue
		} // Do not delete when receipt references are unknown.
		for _, path := range expired {
			if refs[filepath.Base(path)] {
				continue // 仍被某条消息引用，留着
			}
			area, err := s.openDataDir(filepath.Dir(path))
			if err != nil {
				continue
			}
			err = area.RemoveAll(filepath.Base(path))
			area.Close()
			if err != nil {
				log.Printf("image janitor: %v", err)
			}
		}
	}
}
