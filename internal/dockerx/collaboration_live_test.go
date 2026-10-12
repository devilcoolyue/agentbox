package dockerx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
	"agentbox/internal/store"
	"github.com/docker/docker/api/types/container"
)

// Uses only a fresh temporary fixture and a named local image. Docker must see
// the host's temporary directory (native Linux or Docker Desktop file sharing).
// Missing sharing is a failure, never evidence that mount isolation passed.
func TestCollaborationMountProfileLive(t *testing.T) {
	image := os.Getenv("AGENTBOX_COLLABORATION_MOUNT_IMAGE")
	if image == "" {
		t.Skip("requires a local image and Docker access to a private temporary fixture")
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
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, name := range []string{"project", "inputs", "main-home", "worker-home", "main-scratch", "worker-scratch"} {
		p := filepath.Join(root, name)
		if err = os.Mkdir(p, 0777); err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(p, 0777); err != nil {
			t.Fatal(err)
		}
		paths[name] = p
	}
	if err = os.WriteFile(filepath.Join(paths["project"], "source.txt"), []byte("original"), 0666); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(paths["inputs"], "fixture.txt"), []byte("read-only"), 0666); err != nil {
		t.Fatal(err)
	}
	acct := config.Account{ID: "synthetic", Type: "codex"}
	var ids []string
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, id := range ids {
			if err := m.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
				t.Error("fixture cleanup failed", err)
			}
		}
	}()
	run := store.NewCollaborationID()
	for _, role := range []collaboration.Role{collaboration.Coordinator, collaboration.Executor} {
		name := "main"
		if role == collaboration.Executor {
			name = "worker"
		}
		sess := store.Session{ID: store.NewCollaborationID(), User: "synthetic", Agent: "codex", AccountID: acct.ID}
		profile := CollaborationProfile{RunID: run, ParticipantID: sess.ID, Role: role, ImageID: img.ID, Workspace: paths["project"], Inputs: paths["inputs"], Home: paths[name+"-home"], Scratch: paths[name+"-scratch"]}
		id, err := m.EnsureCollaborationRunning(t.Context(), sess, acct, profile)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		sess.ContainerID = id
		if _, err = m.EnsureCollaborationRunning(t.Context(), sess, acct, profile); err == nil {
			t.Fatal("running participant reused")
		}
		command := `set -eu; test "$(id -u)" = 1000; test "$(cat /workspace/source.txt)" = original; test "$(cat /task-inputs/fixture.txt)" = read-only; printf local > /home/agent/local; printf scratch > /shared/local; if printf bad > /task-inputs/fixture.txt 2>/dev/null; then exit 21; fi; `
		if role == collaboration.Coordinator {
			command += `if printf bad > /workspace/source.txt 2>/dev/null; then exit 22; fi; `
		} else {
			command += `printf implemented > /workspace/source.txt; `
		}
		command += `printf passed`
		out, err := m.ExecCapture(t.Context(), id, []string{"/bin/sh", "-c", command}, nil, "")
		if err != nil || !strings.Contains(out, "passed") {
			t.Fatal(out, err)
		}
		if err = m.Stop(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		info, err := m.cli.ContainerInspect(t.Context(), id)
		if err != nil || info.State == nil || info.State.Running {
			t.Fatal("stop unconfirmed", err)
		}
	}
	got, err := os.ReadFile(filepath.Join(paths["project"], "source.txt"))
	if err != nil || string(got) != "implemented" {
		t.Fatal(string(got), err)
	}
	t.Log("actual UID1000 coordinator read-only project, executor write, read-only inputs, private writable home/scratch, running reuse refusal and container stop passed")
}
