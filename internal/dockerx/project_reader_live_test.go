package dockerx

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/gitx"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"github.com/docker/docker/api/types/container"
)

func TestCollaborationProjectImportLive(t *testing.T) {
	image := os.Getenv("AGENTBOX_COLLABORATION_IMPORT_IMAGE")
	if image == "" {
		t.Skip("requires local image and Docker-visible private fixture; no model calls")
	}
	m, err := New(&config.Config{Container: config.ContainerLimits{MemoryMB: 512, CPUs: 1, PidsLimit: 128, Network: "none"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	img, err := m.InspectCLIImage(t.Context(), image)
	if err != nil {
		t.Fatal(err)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"project", "home", "scratch", "inputs", "import"} {
		if err = os.Mkdir(filepath.Join(base, dir), 0777); err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(filepath.Join(base, dir), 0777); err != nil {
			t.Fatal(err)
		}
	}
	sess := store.Session{ID: store.NewCollaborationID(), User: "fixture", Agent: "codex", AccountID: "fixture"}
	profile := CollaborationProfile{RunID: store.NewCollaborationID(), ParticipantID: sess.ID, Role: collaboration.Executor, ImageID: img.ID, Workspace: filepath.Join(base, "project"), Home: filepath.Join(base, "home"), Scratch: filepath.Join(base, "scratch"), Inputs: filepath.Join(base, "inputs")}
	id, err := m.EnsureCollaborationRunning(t.Context(), sess, config.Account{ID: "fixture", Type: "codex"}, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if e := m.Remove(ctx, id); e != nil {
			t.Error(e)
		}
	}()
	// Production workspace roots are UID1000. Docker Desktop's bind-mounted
	// temporary directory starts with the Mac UID; correct only this fixture
	// root rather than weakening Git's ownership check in the product runner.
	chown, err := m.cli.ContainerExecCreate(t.Context(), id, container.ExecOptions{User: "0", Cmd: []string{"/bin/chown", "1000:1000", "/workspace"}, AttachStdout: true, AttachStderr: true})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := m.cli.ContainerExecAttach(t.Context(), chown.ID, container.ExecAttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, stream.Reader)
	stream.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if code, e := m.ExitCode(t.Context(), chown.ID); e != nil || code != 0 {
		t.Fatal("fixture chown failed", code, e)
	}
	setup := `set -eu; git init -q; git config user.name Fixture; git config user.email fixture@example.com; mkdir src; printf fixture > src/file; ln -s src/file alias; git add .; git commit -qm fixture; git rev-parse HEAD`
	head, err := m.ExecCapture(t.Context(), id, []string{"/bin/sh", "-c", setup}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	head = strings.TrimSpace(head)
	if err = m.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	root, err := safefs.Open(filepath.Join(base, "import"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = m.WithProjectReader(t.Context(), img.ID, profile.Workspace, func(readerID string) error {
		if _, e := m.ExecCommand(t.Context(), readerID, []string{"/bin/sh", "-c", "printf bad > /workspace/src/file"}); e == nil {
			return errors.New("source writable in reader")
		}
		git := gitx.New(m, func(context.Context, string) (string, func(), error) { return readerID, func() {}, nil })
		raw, e := git.Run(t.Context(), "fixture", ".", "ls-tree", "-rlz", "--full-tree", head)
		if e != nil {
			return e
		}
		limits := collaboration.SnapshotLimits{MaxBytes: 1 << 20, MaxEntries: 100}
		tree, e := collaboration.ParseGitTree([]byte(raw), limits)
		if e != nil {
			return e
		}
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			e := git.Archive(t.Context(), "fixture", ".", head, pw, 2<<20)
			_ = pw.CloseWithError(e)
			done <- e
		}()
		err := collaboration.ImportGitArchive(t.Context(), pr, root, tree, limits)
		_ = pr.CloseWithError(err)
		return errors.Join(err, <-done)
	})
	if err != nil {
		t.Fatal(err)
	}
	file, err := root.ReadAll("src/file", 100)
	if err != nil || string(file) != "fixture" {
		t.Fatal(string(file), err)
	}
	link, err := root.Readlink("alias")
	if err != nil || link != "src/file" {
		t.Fatal(link, err)
	}
	if _, err = root.Lstat(".git"); !os.IsNotExist(err) {
		t.Fatal("source Git config was exported")
	}
	out, err := m.RunCollaborationVerification(t.Context(), img.ID, profile.Workspace, "test -f src/file && printf verified", store.NewCollaborationID())
	if err != nil || out != "verified" {
		t.Fatal("real verification helper", out, err)
	}
	if _, err = m.RunCollaborationVerification(t.Context(), img.ID, profile.Workspace, "exit 17", store.NewCollaborationID()); err == nil {
		t.Fatal("nonzero verification exit accepted")
	}
	t.Log("real offline Git tree/archive stream, read-only source and verified ordinary file/link import passed")
}
