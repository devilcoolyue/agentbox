package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"

	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"agentbox/internal/syncproto"
)

const linuxCrashFile = "crash.bin"
const linuxCrashBefore = "before cross-OS crash\r\n中文\x00"
const linuxCrashUpload = "native sidecar publication\r\n\x00"
const linuxCrashRemoteEdit = "server user edit after native process kill\r\n中文\x00"

// This test-only controller accepts no paths, bytes, shell commands, process
// IDs, or clock adjustments. It can manipulate only its own fixed fixture file.
type linuxPeerCrash struct {
	mu           sync.Mutex
	prepared     bool
	armed        bool
	published    bool
	disconnected bool
	edited       bool
	applyCalls   int
	operation    string
	failure      string
	unblock      chan struct{}
	server       *Server
	session      store.Session
	project      store.ClientProject
	runID        string
}

func (f *linuxPeerCrash) control(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer client-fixture-alice" || f.runID == "" || r.Header.Get("X-Agentbox-Fixture-Run") != f.runID {
		writeErr(w, http.StatusUnauthorized, "fixture identity mismatch")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	filename := filepath.Join(f.server.workspaceDir(f.session), linuxCrashFile)
	switch r.Method + " " + r.URL.Path {
	case "POST /fixture/crash/prepare":
		if f.prepared || r.ContentLength != 0 || f.server.syncLeases().WorkspaceActive(f.session.ID) {
			writeErr(w, http.StatusConflict, "fixture prepare unavailable")
			return
		}
		if err := os.WriteFile(filename, []byte(linuxCrashBefore), 0600); err != nil {
			writeErr(w, http.StatusInternalServerError, "fixture prepare failed")
			return
		}
		f.prepared = true
	case "POST /fixture/crash/arm":
		if !f.prepared || f.armed || f.published || r.ContentLength != 0 || f.server.syncLeases().WorkspaceActive(f.session.ID) {
			writeErr(w, http.StatusConflict, "fixture cannot arm")
			return
		}
		f.armed = true
	case "POST /fixture/crash/edit":
		if !f.published || !f.disconnected || f.edited || r.ContentLength != 0 {
			writeErr(w, http.StatusConflict, "fixture edit must follow disconnected publication")
			return
		}
		if err := os.WriteFile(filename, []byte(linuxCrashRemoteEdit), 0600); err != nil {
			writeErr(w, http.StatusInternalServerError, "fixture edit failed")
			return
		}
		f.edited = true
	case "GET /fixture/crash/status":
	default:
		writeErr(w, http.StatusNotFound, "unknown fixture control")
		return
	}
	result := map[string]any{
		"run_id": f.runID, "prepared": f.prepared, "armed": f.armed,
		"published": f.published, "disconnected": f.disconnected, "edited": f.edited,
		"apply_calls": f.applyCalls, "operation_id": f.operation, "error": f.failure,
		"lease_active":      f.server.syncLeases().Active(f.session.ID, f.project.ID),
		"lease_ttl_seconds": int(syncproto.LeaseTTL / time.Second),
	}
	if f.prepared {
		if bytes, err := os.ReadFile(filename); err == nil {
			result["current_hash"] = syncproto.HashBytes(bytes)
		} else {
			writeErr(w, 500, "fixture current bytes unavailable")
			return
		}
	}
	if f.published && f.failure == "" {
		before, err := os.ReadFile(filepath.Join(f.server.sessionDir(f.session), "client-sync", f.operation, "before"))
		if err != nil {
			writeErr(w, 500, "fixture recovery bytes unavailable")
			return
		}
		result["before_hash"] = syncproto.HashBytes(before)
	}
	writeJSON(w, 200, result)
}

func (f *linuxPeerCrash) apply(w http.ResponseWriter, r *http.Request, next http.Handler) {
	f.mu.Lock()
	f.applyCalls++
	armed := f.armed
	if armed {
		f.armed = false
	}
	f.mu.Unlock()
	if !armed {
		next.ServeHTTP(w, r)
		return
	}
	var mutation syncproto.Mutation
	encoded, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Agentbox-Sync-Request"))
	if err != nil || json.Unmarshal(encoded, &mutation) != nil || mutation.Validate() != nil || mutation.Project != f.project.ID || mutation.Path != linuxCrashFile || mutation.Kind != "replace" || mutation.Before == nil || mutation.After == nil || mutation.Before.Hash != syncproto.HashBytes([]byte(linuxCrashBefore)) || mutation.After.Hash != syncproto.HashBytes([]byte(linuxCrashUpload)) {
		f.mu.Lock()
		f.failure = "armed request did not match the fixed crash fixture"
		f.mu.Unlock()
		writeErr(w, http.StatusBadRequest, "invalid crash fixture request")
		return
	}
	// The production handler really publishes the bytes, saves before, and syncs
	// its applied journal. No response header/body reaches the client afterward.
	response := httptest.NewRecorder()
	next.ServeHTTP(response, r)
	validation := f.verifyPublication(response, mutation)
	f.mu.Lock()
	f.published = true
	f.operation = mutation.ID
	if validation != nil {
		f.failure = validation.Error()
	}
	f.mu.Unlock()
	select {
	case <-r.Context().Done():
		f.mu.Lock()
		f.disconnected = true
		f.mu.Unlock()
	case <-f.unblock:
	}
}

func (f *linuxPeerCrash) verifyPublication(response *httptest.ResponseRecorder, mutation syncproto.Mutation) error {
	var result syncproto.MutationResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.ID != mutation.ID || result.Status != "applied" || !result.Recovery {
		return fmt.Errorf("real handler did not confirm applied recovery: HTTP %d", response.Code)
	}
	operation, err := safefs.Open(filepath.Join(f.server.sessionDir(f.session), "client-sync", mutation.ID))
	if err != nil {
		return fmt.Errorf("publication journal unavailable")
	}
	defer operation.Close()
	record, err := loadClientMutationRecord(operation, mutation.ID)
	if err != nil || record.Result.Status != "applied" || !record.Result.Recovery {
		return fmt.Errorf("publication receipt not durably readable")
	}
	before, err := operation.ReadAll("before", 1024)
	if err != nil || string(before) != linuxCrashBefore {
		return fmt.Errorf("publication did not preserve exact before bytes")
	}
	current, err := os.ReadFile(filepath.Join(f.server.workspaceDir(f.session), linuxCrashFile))
	if err != nil || string(current) != linuxCrashUpload {
		return fmt.Errorf("publication did not replace current file")
	}
	return nil
}

func TestLinuxPeerCrashControlsRejectScopeAndArbitraryInput(t *testing.T) {
	s, session, _ := clientTestServer(t)
	f := linuxPeerCrash{server: s, session: session, runID: "isolated-run", unblock: make(chan struct{})}
	request := func(method, path, token, runID, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", token)
		r.Header.Set("X-Agentbox-Fixture-Run", runID)
		w := httptest.NewRecorder()
		f.control(w, r)
		return w
	}
	for _, fields := range []struct{ token, run string }{{"", f.runID}, {"Bearer client-fixture-bob", f.runID}, {"Bearer client-fixture-alice", ""}, {"Bearer client-fixture-alice", "different-run"}} {
		if w := request("POST", "/fixture/crash/prepare", fields.token, fields.run, ""); w.Code != http.StatusUnauthorized {
			t.Fatal("fixture accepted wrong owner/run", w.Code)
		}
	}
	for _, fields := range []struct{ path, body string }{{"/fixture/crash/prepare", `{"path":"/outside","bytes":"injected"}`}, {"/fixture/crash/edit", ""}, {"/fixture/crash/arbitrary", ""}} {
		if w := request("POST", fields.path, "Bearer client-fixture-alice", f.runID, fields.body); w.Code == http.StatusOK {
			t.Fatal("fixture accepted arbitrary input or premature edit")
		}
	}
	if _, err := os.Stat(filepath.Join(s.workspaceDir(session), linuxCrashFile)); !os.IsNotExist(err) {
		t.Fatal("rejected controls created a file", err)
	}
	if w := request("POST", "/fixture/crash/prepare", "Bearer client-fixture-alice", f.runID, ""); w.Code != http.StatusOK {
		t.Fatal("fixed prepare failed", w.Code, w.Body.String())
	}
	bytes, err := os.ReadFile(filepath.Join(s.workspaceDir(session), linuxCrashFile))
	if err != nil || string(bytes) != linuxCrashBefore {
		t.Fatal("fixture did not use fixed synthetic content", err)
	}
}

// Isolated cross-OS peer, compiled only into the Go test binary. Synthetic
// credentials/data, no Docker socket or provider calls. Never production config.
func TestClientLinuxPeer(t *testing.T) {
	if os.Getenv("AGENTBOX_LINUX_PEER") != "1" {
		t.Skip("explicit isolated Linux peer only")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("cross-OS peer requires an actual Linux runtime")
	}
	s, sess, handler := clientTestServer(t)
	project, err := s.store.CreateClientProject(store.ClientProject{SessionID: sess.ID, Name: "Cross OS", Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.workspaceDir(sess)+"/remote.txt", []byte("linux\r\n中文\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	crash := &linuxPeerCrash{server: s, session: sess, project: project, runID: os.Getenv("AGENTBOX_LINUX_PEER_RUN"), unblock: make(chan struct{})}
	defer close(crash.unblock)
	wrapper := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/fixture/crash/") {
			crash.control(w, r)
			return
		}
		if r.URL.Path == "/api/clients/capabilities" {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, r)
			var identity syncproto.ServerIdentity
			if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &identity) != nil {
				w.WriteHeader(rr.Code)
				return
			}
			identity.Features["sync"] = 1
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(identity)
			return
		}
		if r.URL.Path == "/fixture" {
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer client-fixture-alice" || crash.runID == "" || r.Header.Get("X-Agentbox-Fixture-Run") != crash.runID {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{
				"workspace": sess.ID, "project": project.ID, "os": runtime.GOOS,
				"arch": runtime.GOARCH, "run_id": os.Getenv("AGENTBOX_LINUX_PEER_RUN"),
			})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/sessions/"+sess.ID+"/sync/apply" && r.Header.Get("Authorization") == "Bearer client-fixture-alice" {
			crash.apply(w, r, handler)
			return
		}
		handler.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", "0.0.0.0:8181")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: wrapper, ReadHeaderTimeout: 10 * time.Second}
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err = <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Minute):
	case <-t.Context().Done():
	}
	server.Shutdown(context.Background())
}
