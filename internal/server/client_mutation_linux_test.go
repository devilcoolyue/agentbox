//go:build linux

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"agentbox/internal/dockerx"
	"agentbox/internal/syncproto"
)

// CI runs this explicitly as its ordinary user, before the complete server
// suite runs with production ownership permissions. Root or an account allowed
// to chown to the container UID/GID cannot exercise this denial.
func TestSyncMutationLinuxChownDeniedPreservesTarget(t *testing.T) {
	unavailable := func(reason string) {
		t.Helper()
		if os.Getenv("AGENTBOX_CHOWN_DENIAL_TEST") == "1" {
			t.Fatal(reason)
		}
		t.Skip(reason)
	}
	if os.Geteuid() == 0 {
		unavailable("requires an ordinary Linux user without container ownership permissions")
	}
	probe := filepath.Join(t.TempDir(), "chown-probe")
	if err := os.WriteFile(probe, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(probe, dockerx.AgentUID, dockerx.AgentGID); err == nil {
		unavailable("current UID/GID can chown to the container owner; no denial to test")
	} else if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("unexpected ownership probe failure: %v", err)
	}

	s, sess, handler, lease, operation := mutationFixture(t)
	file := filepath.Join(s.workspaceDir(sess), operation.Path)
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	owner := original.Sys().(*syscall.Stat_t)
	operation.Before = mutationEntry("original")
	assertPreserved := func(expected string) {
		t.Helper()
		contents, err := os.ReadFile(file)
		if err != nil || string(contents) != expected {
			t.Fatalf("target bytes changed: %q, %v", contents, err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		current := info.Sys().(*syscall.Stat_t)
		if !os.SameFile(original, info) || current.Uid != owner.Uid || current.Gid != owner.Gid || info.Mode() != original.Mode() {
			t.Fatal("denied mutation replaced the original file or changed its ownership/mode")
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		response := mutationRequest(handler, operation, lease, strings.NewReader("new\r\n\x00"), "alice")
		var rejected struct {
			Operation syncproto.MutationResult `json:"operation"`
		}
		if response.Code != http.StatusConflict || json.Unmarshal(response.Body.Bytes(), &rejected) != nil || rejected.Operation.ID != operation.ID || rejected.Operation.Status != "uncertain" || rejected.Operation.Recovery || rejected.Operation.Replayed != (attempt != 0) {
			t.Fatalf("unexpected denied mutation response: %d %s", response.Code, response.Body.String())
		}
		if attempt == 0 {
			assertPreserved("original")
			// A repeated operation must keep its uncertain receipt and preserve an
			// editor's later bytes, rather than retrying publication under that ID.
			if err := os.WriteFile(file, []byte("later editor change"), 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			assertPreserved("later editor change")
		}
	}
}
