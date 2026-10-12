package server

import (
	"archive/zip"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/store"
)

func (s *Server) handleCollaborationDownload(w http.ResponseWriter, r *http.Request) {
	if !s.collaborationEnabled(w, r) {
		return
	}
	u := reqUser(r)
	run, err := s.collaborationRun(u, r.PathValue("run"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	inspection := r.URL.Query().Get("snapshot")
	if run.State != store.CollabCompleted && inspection == "" {
		writeCollaborationError(w, r, collaboration.ErrState)
		return
	}
	artifact, err := s.store.CollaborationLatestArtifact(u, run.ID)
	if inspection != "" {
		artifact, err = s.store.CollaborationArtifact(u, run.ID, inspection)
		if err == nil && artifact.Kind != "inspection" {
			err = collaboration.ErrInvalid
		}
	}
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	root, err := s.openDataDir(filepath.Join(s.collaborationDir(u, run.ID), "snapshots"))
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	defer root.Close()
	snapshot, err := collaboration.VerifySnapshot(r.Context(), root, artifact.ID, artifact.SHA256, collaborationSnapshotLimits)
	if err != nil {
		writeProblem(w, r, "collaboration.download", "internal_error")
		return
	}
	tree, err := root.Sub(artifact.ID + "/tree")
	if err != nil {
		writeCollaborationError(w, r, err)
		return
	}
	defer tree.Close()
	w.Header().Set("Content-Type", "application/zip")
	name := "collaboration-" + run.ID
	if inspection != "" {
		name += "-inspection-" + artifact.ID
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.zip"`)
	zw := zip.NewWriter(w)
	for _, entry := range snapshot.Manifest.Entries {
		if r.Context().Err() != nil {
			return
		}
		h := &zip.FileHeader{Name: entry.Path, Method: zip.Deflate, Modified: time.Unix(0, 0)}
		if entry.Kind == "directory" {
			h.Name += "/"
			h.Method = zip.Store
		}
		if entry.Kind == "symlink" {
			h.SetMode(0777 | os.ModeSymlink)
		} else if entry.Kind == "directory" {
			h.SetMode(0755 | os.ModeDir)
		} else {
			h.SetMode(os.FileMode(entry.Mode))
			if entry.Mode&0111 != 0 {
				h.SetMode(0755)
			}
		}
		dst, e := zw.CreateHeader(h)
		if e != nil {
			return
		}
		switch entry.Kind {
		case "file":
			f, e := tree.OpenFile(entry.Path)
			if e != nil {
				return
			}
			_, e = io.Copy(dst, io.LimitReader(f, entry.Size))
			closeErr := f.Close()
			if errors.Join(e, closeErr) != nil {
				return
			}
		case "symlink":
			if _, e = io.WriteString(dst, entry.Target); e != nil {
				return
			}
		}
	}
	_ = zw.Close()
}
