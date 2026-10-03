//go:build darwin || linux

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"agentbox/internal/syncclient"
	"agentbox/internal/syncproto"
)

// This opt-in test builds the real production sidecar, connects it to the real
// Go HTTP routes over TCP, then sends SIGKILL at a server-committed / client-
// unacknowledged boundary. It is not os.Exit, a mocked executor, fault injection
// into storage, or a claim about physical power-loss durability.
func TestSyncSidecarSIGKILLAfterRemotePublication(t *testing.T) {
	if os.Getenv("AGENTBOX_SYNC_PROCESS_TEST") != "1" {
		t.Skip("set AGENTBOX_SYNC_PROCESS_TEST=1 to build and SIGKILL the isolated native sidecar")
	}
	binary := buildKilledSidecar(t)
	var armed atomic.Bool
	var applyCalls atomic.Int32
	type publication struct {
		request syncproto.Mutation
		result  syncproto.MutationResult
		err     error
	}
	published := make(chan publication, 1)
	disconnected := make(chan struct{}, 1)
	unblock := make(chan struct{})
	defer close(unblock)
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/sync/apply") {
				next.ServeHTTP(w, r)
				return
			}
			applyCalls.Add(1)
			if !armed.Swap(false) {
				next.ServeHTTP(w, r)
				return
			}
			var state publication
			metadata, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Agentbox-Sync-Request"))
			if err != nil {
				state.err = err
			} else if err = json.Unmarshal(metadata, &state.request); err != nil {
				state.err = err
			}
			// Real handler completion means publication, before-copy and journal
			// persistence have finished. Hold all response bytes back from TCP.
			response := httptest.NewRecorder()
			next.ServeHTTP(response, r)
			if response.Code != http.StatusOK {
				state.err = fmt.Errorf("fixture apply returned %d", response.Code)
			} else if err := json.Unmarshal(response.Body.Bytes(), &state.result); err != nil {
				state.err = err
			}
			published <- state
			select {
			case <-r.Context().Done():
				disconnected <- struct{}{}
			case <-unblock:
			}
		})
	})
	// The fixture registers the native picker directory. Every preview,
	// baseline commit, upload and recovery command below runs in the sidecar.
	if err := f.engine.State.Close(); err != nil {
		t.Fatal(err)
	}
	const original = "before publication\r\n\x00"
	writeEngineFile(t, f.local, "file.bin", original)
	writeEngineFile(t, f.remote, "file.bin", original)
	first := startKilledSidecar(t, binary, f)
	initial := first.call(t, "sync_preview", nil)
	if initial.Preview == nil || len(initial.Preview.Plan.Operations) != 0 {
		t.Fatal("equal starting trees did not produce an empty baseline plan")
	}
	first.call(t, "sync_apply", map[string]any{"preview": initial.Preview, "confirmation": initial.Preview.Plan.Digest})
	baseline := loadKilledSidecarBinding(t, f)
	if baseline.Baseline == nil || baseline.Pending != nil || applyCalls.Load() != 0 {
		t.Fatal("native sidecar did not establish the initial baseline")
	}

	writeEngineFile(t, f.local, "file.bin", "sidecar upload\r\n\x00")
	preview := first.call(t, "sync_preview", nil)
	if preview.Preview == nil || len(preview.Preview.Plan.Operations) != 1 || preview.Preview.Plan.Operations[0].Kind != "upload" {
		t.Fatal("expected exactly one native upload")
	}
	armed.Store(true)
	first.send(t, "sync_apply", map[string]any{"preview": preview.Preview, "confirmation": preview.Preview.Plan.Digest})
	var committed publication
	select {
	case committed = <-published:
	case <-time.After(30 * time.Second):
		t.Fatal("native sidecar never reached the server publication boundary")
	}
	if committed.err != nil || committed.result.Status != "applied" || !committed.result.Recovery || committed.result.ID != committed.request.ID {
		t.Fatal("server did not durably publish replacement with recovery", committed.err, committed.result)
	}
	started := loadKilledSidecarBinding(t, f)
	if started.Pending == nil || len(started.Pending.Operations) != 1 || started.Pending.Operations[0].Status != "started" || started.Pending.Operations[0].ID != committed.request.ID {
		t.Fatal("real sidecar did not persist started intent before sending bytes")
	}
	if !reflect.DeepEqual(started.Baseline, baseline.Baseline) {
		t.Fatal("unacknowledged upload advanced the baseline")
	}
	t.Log("real HTTP replacement and before-copy committed; client started intent read from SQLite; response remains withheld")
	first.kill(t)
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGKILL did not disconnect the real HTTP transport")
	}
	afterKill := loadKilledSidecarBinding(t, f)
	if !reflect.DeepEqual(started, afterKill) {
		t.Fatal("kernel-killed process lost or rewrote its durable pending intent")
	}
	if !f.server.syncLeases().Active(f.session.ID, f.project.ID) {
		t.Fatal("SIGKILL unexpectedly ran graceful lease cleanup")
	}

	// Real local and server edits after the crash must survive all recovery.
	const localEdit = "user local edit after SIGKILL"
	const remoteEdit = "user server edit after SIGKILL"
	writeEngineFile(t, f.local, "file.bin", localEdit)
	writeEngineFile(t, f.remote, "file.bin", remoteEdit)
	second := startKilledSidecar(t, binary, f)
	if second.cmd.Process.Pid == first.cmd.Process.Pid {
		t.Fatal("recovery did not run in a new OS process")
	}
	listed := second.call(t, "sync_list", nil)
	if len(listed.Bindings) != 1 || !listed.Bindings[0].Pending || listed.Bindings[0].ID != f.saved.ID {
		t.Fatal("new sidecar process did not recover pending binding")
	}
	refused := second.exchange(t, "sync_apply", map[string]any{"preview": preview.Preview, "confirmation": preview.Preview.Plan.Digest})
	if refused.Type != "error" || refused.Error != "sync_pending" || applyCalls.Load() != 1 {
		t.Fatal("restart replayed ambiguous operation", refused.Type, refused.Error, applyCalls.Load())
	}
	refused = second.exchange(t, "sync_review", nil)
	if refused.Type != "error" || !f.server.syncLeases().Active(f.session.ID, f.project.ID) {
		t.Fatal("recovery bypassed the dead process's still-active lease")
	}
	t.Log("new process retained pending and refused replay; waiting for the real 30-second lease to expire")
	deadline := time.NewTimer(syncproto.LeaseTTL + 5*time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for f.server.syncLeases().Active(f.session.ID, f.project.ID) {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("crashed process lease did not expire naturally")
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	reviewed := second.call(t, "sync_review", nil)
	review := reviewed.Review
	if review == nil || review.CanFinish || review.FinalMatches || len(review.Items) != 1 {
		t.Fatal("post-crash user edits were incorrectly considered the intended result")
	}
	item := review.Items[0]
	if item.ID != committed.request.ID || item.Recorded != "started" || item.Receipt != "applied" || item.Current != "diverged" || !item.Recovery {
		t.Fatal("review lost original operation receipt or before-copy", item)
	}
	refused = second.exchange(t, "sync_resolve", map[string]any{"action": "finish", "confirmation": review.Digest})
	if refused.Type != "error" || refused.Error != "sync_pending" {
		t.Fatal("diverged post-crash files were silently committed")
	}
	exportDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exported := second.call(t, "sync_export", map[string]any{"batch_id": started.Pending.ID, "operation_id": committed.request.ID, "export_directory": exportDir})
	before, err := os.ReadFile(filepath.Join(exportDir, exported.Filename))
	if err != nil || string(before) != original {
		t.Fatal("fresh sidecar could not export byte-exact pre-crash recovery", err)
	}
	resolved := second.call(t, "sync_resolve", map[string]any{"action": "replan", "confirmation": review.Digest})
	if resolved.Binding == nil || resolved.Binding.Pending {
		t.Fatal("explicit replan did not archive pending")
	}
	final := loadKilledSidecarBinding(t, f)
	if final.Pending != nil || !reflect.DeepEqual(final.Baseline, baseline.Baseline) {
		t.Fatal("replan advanced the old baseline")
	}
	store, err := syncclient.OpenState(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	history, err := store.History(f.saved.ID, started.Pending.ID)
	store.Close()
	if err != nil || history.Resolution == nil || history.Resolution.Action != "replan" || history.Operations[0].ID != committed.request.ID || history.Operations[0].Status != "started" {
		t.Fatal("archived recovery lost original started intent", err)
	}
	for _, expected := range []struct{ root, bytes string }{{f.local, localEdit}, {f.remote, remoteEdit}} {
		data, err := os.ReadFile(filepath.Join(expected.root, "file.bin"))
		if err != nil || string(data) != expected.bytes {
			t.Fatal("recovery changed a user's post-crash edit", err)
		}
	}
	replanned := second.call(t, "sync_preview", nil)
	if replanned.Preview == nil || len(replanned.Preview.Plan.Conflicts) != 1 || len(replanned.Preview.Plan.Operations) != 0 || applyCalls.Load() != 1 {
		t.Fatal("post-crash edits did not remain a conflict, or mutation was replayed")
	}
	second.stop(t)
	t.Log("SIGKILL recovery passed: original ID retained, one HTTP apply total, before bytes exported, both user edits preserved, old baseline unchanged")
}

type killedSidecarEvent struct {
	Version  int                       `json:"version"`
	ID       string                    `json:"id"`
	Type     string                    `json:"type"`
	Error    string                    `json:"error"`
	Binding  *syncclient.BindingView   `json:"binding"`
	Bindings []syncclient.BindingView  `json:"bindings"`
	Preview  *syncclient.Preview       `json:"preview"`
	Review   *syncclient.PendingReview `json:"review"`
	Filename string                    `json:"filename"`
}

type killedSidecarRead struct {
	event killedSidecarEvent
	err   error
}

type killedSidecar struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	encoder *json.Encoder
	events  chan killedSidecarRead
	done    chan struct{}
	err     error // Read only after done is closed.
	fixture *engineFixture
	nextID  int
}

func buildKilledSidecar(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "abox-sync")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", binary, "./cmd/abox-sync")
	command.Dir = filepath.Join("..", "..")
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build production sidecar: %v\n%s", err, output)
	}
	return binary
}

func startKilledSidecar(t *testing.T, binary string, fixture *engineFixture) *killedSidecar {
	t.Helper()
	p := &killedSidecar{cmd: exec.Command(binary, "--desktop"), events: make(chan killedSidecarRead, 16), done: make(chan struct{}), fixture: fixture}
	var err error
	p.input, err = p.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.cmd.Stderr = io.Discard
	p.encoder = json.NewEncoder(p.input)
	if err = p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			var event killedSidecarEvent
			err := json.Unmarshal(scanner.Bytes(), &event)
			if bytes.Contains(scanner.Bytes(), []byte("client-fixture-alice")) || bytes.Contains(scanner.Bytes(), []byte("lease_token")) {
				err = errors.New("credential escaped to sidecar stdout")
			}
			p.events <- killedSidecarRead{event, err}
		}
		p.events <- killedSidecarRead{err: io.EOF}
	}()
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		p.input.Close()
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
				t.Error("native sidecar remained alive during test cleanup")
			}
		}
	})
	ready := p.receive(t, "")
	if ready.Version != 1 || ready.Type != "ready" {
		t.Fatal("production sidecar handshake failed")
	}
	return p
}

func (p *killedSidecar) send(t *testing.T, kind string, fields map[string]any) string {
	t.Helper()
	p.nextID++
	id := fmt.Sprintf("native-%d", p.nextID)
	request := map[string]any{"version": 1, "id": id, "type": kind, "server": p.fixture.saved.Binding.Server, "user": "alice", "token": "client-fixture-alice", "state_dir": p.fixture.stateDir, "binding_id": p.fixture.saved.ID}
	for key, value := range fields {
		request[key] = value
	}
	if err := p.encoder.Encode(request); err != nil {
		t.Fatal("write private sidecar pipe", err)
	}
	return id
}

func (p *killedSidecar) receive(t *testing.T, id string) killedSidecarEvent {
	t.Helper()
	select {
	case result := <-p.events:
		if result.err != nil || result.event.ID != id {
			t.Fatal("read private sidecar pipe", result.err, result.event.Type)
		}
		return result.event
	case <-time.After(30 * time.Second):
		t.Fatal("sidecar IPC response timed out")
	}
	return killedSidecarEvent{}
}

func (p *killedSidecar) exchange(t *testing.T, kind string, fields map[string]any) killedSidecarEvent {
	t.Helper()
	return p.receive(t, p.send(t, kind, fields))
}

func (p *killedSidecar) call(t *testing.T, kind string, fields map[string]any) killedSidecarEvent {
	t.Helper()
	result := p.exchange(t, kind, fields)
	if result.Type == "error" {
		t.Fatal("sidecar command failed", kind, result.Error)
	}
	return result
}

func (p *killedSidecar) kill(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal("send kernel SIGKILL", err)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGKILL did not terminate sidecar")
	}
	var exit *exec.ExitError
	if !errors.As(p.err, &exit) {
		t.Fatal("sidecar exited without signal status", p.err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("sidecar was not killed by the kernel with SIGKILL", exit)
	}
}

func (p *killedSidecar) stop(t *testing.T) {
	t.Helper()
	if event := p.call(t, "shutdown", nil); event.Type != "stopped" {
		t.Fatal("restarted sidecar did not stop normally")
	}
	select {
	case <-p.done:
		if p.err != nil {
			t.Fatal(p.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restarted sidecar shutdown timed out")
	}
}

func loadKilledSidecarBinding(t *testing.T, f *engineFixture) syncclient.SavedBinding {
	t.Helper()
	state, err := syncclient.OpenState(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	saved, err := state.Load(f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}
