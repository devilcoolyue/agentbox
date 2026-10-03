package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/syncclient"
	"agentbox/internal/syncproto"
)

// Simulate a published mutation whose response was lost, without retrying it.
func lostEngineResponse(t *testing.T, files map[string]string) (*engineFixture, *atomic.Int32) {
	t.Helper()
	var fail atomic.Bool
	fail.Store(true)
	calls := new(atomic.Int32)
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/sync/apply") {
				calls.Add(1)
				if fail.Swap(false) {
					response := httptest.NewRecorder()
					next.ServeHTTP(response, r)
					if response.Code != 200 {
						t.Error(response.Code, response.Body.String())
					}
					w.Header().Set("X-Agentbox-Server-ID", response.Header().Get("X-Agentbox-Server-ID"))
					w.WriteHeader(503)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	for name, data := range files {
		writeEngineFile(t, f.local, name, data)
	}
	p, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, p, p.Plan.Digest); err == nil {
		t.Fatal("fixture did not interrupt batch")
	}
	return f, calls
}
func TestSyncReconcileLostResponseCommitsWithoutAnotherMutation(t *testing.T) {
	f, calls := lostEngineResponse(t, map[string]string{"file": "bytes"})
	review, err := f.engine.ReviewPending(t.Context(), f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !review.CanFinish || !review.FinalMatches || len(review.Items) != 1 || review.Items[0].Recorded != "started" || review.Items[0].Receipt != "applied" {
		t.Fatal(review)
	}
	before := calls.Load()
	saved, err := f.engine.ResolvePending(t.Context(), f.saved.ID, review.Digest, "finish")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Pending != nil || saved.Baseline == nil || calls.Load() != before {
		t.Fatal("resolution replayed or lost baseline")
	}
	history, err := f.engine.State.History(saved.ID, review.BatchID)
	if err != nil || history.Resolution == nil || history.Resolution.Action != "finish" || history.Resolution.ReviewDigest != review.Digest {
		t.Fatal("resolution not archived", err)
	}
	if history.Operations[0].Status != "started" {
		t.Fatal("rewrote historical operation acknowledgement")
	}
	preview, err := f.engine.Preview(t.Context(), saved.ID, syncclient.Automatic)
	if err != nil || len(preview.Plan.Operations) != 0 {
		t.Fatal("baseline did not converge", err)
	}
	if _, err = f.engine.ResolvePending(t.Context(), saved.ID, review.Digest, "finish"); !errors.Is(err, syncclient.ErrStateChanged) {
		t.Fatal("resolution replay accepted", err)
	}
}
func TestSyncReconcilePartialBatchReplansWithoutChangingOldBaseline(t *testing.T) {
	f, calls := lostEngineResponse(t, map[string]string{"a": "first", "b": "second"})
	review, err := f.engine.ReviewPending(t.Context(), f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if review.CanFinish || review.FinalMatches {
		t.Fatal("partial batch considered complete")
	}
	if _, err = f.engine.ResolvePending(t.Context(), f.saved.ID, review.Digest, "finish"); !errors.Is(err, syncclient.ErrPending) {
		t.Fatal("partial finish accepted", err)
	}
	before := calls.Load()
	saved, err := f.engine.ResolvePending(t.Context(), f.saved.ID, review.Digest, "replan")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Pending != nil || saved.Baseline != nil || calls.Load() != before {
		t.Fatal("replan advanced baseline or mutated files")
	}
	history, err := f.engine.State.History(saved.ID, review.BatchID)
	if err != nil || history.Resolution.Action != "replan" {
		t.Fatal(history, err)
	}
	preview, err := f.engine.Preview(t.Context(), saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Plan.Operations) != 1 || preview.Plan.Operations[0].Path != "b" || !preview.Plan.NeedsConfirmation {
		t.Fatal(preview)
	}
	if _, err = f.engine.Apply(t.Context(), saved.ID, preview, preview.Plan.Digest); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != before+1 {
		t.Fatal("replayed already applied upload")
	}
}
func TestSyncReconcileRejectsStaleReviewAndLiveLease(t *testing.T) {
	f, _ := lostEngineResponse(t, map[string]string{"file": "bytes"})
	review, err := f.engine.ReviewPending(t.Context(), f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeEngineFile(t, f.remote, "file", "external edit")
	for _, action := range []string{"finish", "replan"} {
		if _, err = f.engine.ResolvePending(t.Context(), f.saved.ID, review.Digest, action); !errors.Is(err, syncclient.ErrStateChanged) {
			t.Fatal("stale review accepted", action, err)
		}
	}
	saved, err := f.engine.State.Load(f.saved.ID)
	if err != nil || saved.Pending == nil {
		t.Fatal(err)
	}
	lease, err := f.engine.Remote.Acquire(t.Context(), f.session.ID, f.project.ID, "active-writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.ReviewPending(t.Context(), f.saved.ID); err == nil {
		t.Fatal("review bypassed another live writer")
	}
	if err = f.engine.Remote.Release(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	changed, err := f.engine.ReviewPending(t.Context(), f.saved.ID)
	if err != nil || changed.CanFinish || changed.Items[0].Current != "diverged" {
		t.Fatal(changed, err)
	}
}
func TestSyncReconcileReceiptFailureNeverClearsPending(t *testing.T) {
	var breakReceipt atomic.Bool
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if breakReceipt.Load() && strings.Contains(r.URL.Path, "/sync/operations/") {
				w.WriteHeader(500)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/sync/apply") {
				response := httptest.NewRecorder()
				next.ServeHTTP(response, r)
				w.Header().Set("X-Agentbox-Server-ID", response.Header().Get("X-Agentbox-Server-ID"))
				w.WriteHeader(503)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	writeEngineFile(t, f.local, "file", "bytes")
	p, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, p, p.Plan.Digest); err == nil {
		t.Fatal("expected interruption")
	}
	breakReceipt.Store(true)
	if _, err = f.engine.ReviewPending(t.Context(), f.saved.ID); err == nil {
		t.Fatal("receipt failure treated as absent")
	}
	saved, err := f.engine.State.Load(f.saved.ID)
	if err != nil || saved.Pending == nil {
		t.Fatal("failure cleared pending", err)
	}
}
func TestSyncRecoveryExportsBothSidesAndPreservesExistingDestination(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	for _, name := range []string{"download", "upload"} {
		writeEngineFile(t, f.local, name, "old\r\n\x00")
		writeEngineFile(t, f.remote, name, "old\r\n\x00")
	}
	engineRound(t, f)
	writeEngineFile(t, f.local, "upload", "local new")
	writeEngineFile(t, f.remote, "download", "remote new")
	engineRound(t, f)
	history, err := f.engine.RecoveryHistory(t.Context(), f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || len(history[0].Files) != 2 {
		t.Fatal(history)
	}
	destination, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range history[0].Files {
		name, err := f.engine.ExportRecovery(t.Context(), f.saved.ID, history[0].ID, item.ID, destination)
		if err != nil {
			t.Fatal(item.Side, err)
		}
		raw, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || string(raw) != "old\r\n\x00" {
			t.Fatal("recovery bytes lost", err)
		}
		if err = os.WriteFile(filepath.Join(destination, name), []byte("keep existing"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = f.engine.ExportRecovery(t.Context(), f.saved.ID, history[0].ID, item.ID, destination); err == nil {
			t.Fatal("existing export overwritten")
		}
		raw, err = os.ReadFile(filepath.Join(destination, name))
		if err != nil || string(raw) != "keep existing" {
			t.Fatal("existing export changed", err)
		}
		if _, err = f.engine.ExportRecovery(t.Context(), f.saved.ID, history[0].ID, item.ID, f.local); err == nil {
			t.Fatal("export allowed into mapped tree")
		}
	}
	// Export never changed the latest file contents or initiated another batch.
	for _, pair := range []struct{ root, name, want string }{{f.local, "download", "remote new"}, {f.remote, "upload", "local new"}} {
		raw, err := os.ReadFile(filepath.Join(pair.root, pair.name))
		if err != nil || string(raw) != pair.want {
			t.Fatal("export restored over live data", err)
		}
	}
}
func TestSyncRecoveryBadLocalCopyCannotBePublished(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	writeEngineFile(t, f.local, "file", "old")
	writeEngineFile(t, f.remote, "file", "old")
	engineRound(t, f)
	writeEngineFile(t, f.remote, "file", "new")
	engineRound(t, f)
	history, err := f.engine.RecoveryHistory(t.Context(), f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := f.engine.State.History(f.saved.ID, history[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	item := batch.Operations[0]
	if err = os.WriteFile(filepath.Join(f.local, filepath.FromSlash(item.Recovery.Path)), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	destination, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.ExportRecovery(t.Context(), f.saved.ID, batch.ID, item.ID, destination); err == nil {
		t.Fatal("bad recovery exported")
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed export left bytes", err)
	}
	if _, err = f.engine.ExportRecovery(t.Context(), f.saved.ID, batch.ID, strings.Repeat("f", 32), destination); !errors.Is(err, syncproto.ErrInvalid) {
		t.Fatal("foreign operation accepted", err)
	}
}

func TestSyncReconcileUncertainReceiptRequiresExplicitReplan(t *testing.T) {
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/sync/apply") || strings.Contains(r.URL.Path, "/sync/operations/") {
				response := httptest.NewRecorder()
				next.ServeHTTP(response, r)
				for k, values := range response.Header() {
					w.Header()[k] = values
				}
				if strings.HasSuffix(r.URL.Path, "/sync/apply") {
					w.WriteHeader(503)
					return
				}
				var status syncproto.OperationStatus
				if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				status.Operation.Status = "uncertain"
				_ = json.NewEncoder(w).Encode(status)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	writeEngineFile(t, f.local, "file", "new")
	preview, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest); err == nil {
		t.Fatal("expected lost response")
	}
	review, err := f.engine.ReviewPending(t.Context(), f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !review.FinalMatches || review.CanFinish || review.Items[0].Receipt != "uncertain" {
		t.Fatal("uncertainty hidden", review)
	}
	if _, err = f.engine.ResolvePending(t.Context(), f.saved.ID, review.Digest, "finish"); !errors.Is(err, syncclient.ErrPending) {
		t.Fatal("uncertain finish accepted", err)
	}
	saved, err := f.engine.ResolvePending(t.Context(), f.saved.ID, review.Digest, "replan")
	if err != nil || saved.Pending != nil || saved.Baseline != nil {
		t.Fatal("replan accepted files as baseline", err)
	}
}
func TestSyncReconcileReplanPreservesBaselineAndLaterConflict(t *testing.T) {
	var fail atomic.Bool
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail.Load() && strings.HasSuffix(r.URL.Path, "/sync/apply") {
				response := httptest.NewRecorder()
				next.ServeHTTP(response, r)
				w.Header().Set("X-Agentbox-Server-ID", response.Header().Get("X-Agentbox-Server-ID"))
				w.WriteHeader(503)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	writeEngineFile(t, f.local, "file", "old")
	writeEngineFile(t, f.remote, "file", "old")
	base := engineRound(t, f)
	writeEngineFile(t, f.local, "file", "local edit")
	fail.Store(true)
	preview, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest); err == nil {
		t.Fatal("expected lost response")
	}
	writeEngineFile(t, f.remote, "file", "remote later edit")
	review, err := f.engine.ReviewPending(t.Context(), f.saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := f.engine.ResolvePending(t.Context(), f.saved.ID, review.Digest, "replan")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := base.Baseline.Local.Digest()
	after, _ := saved.Baseline.Local.Digest()
	beforeRemote, _ := base.Baseline.Remote.Digest()
	afterRemote, _ := saved.Baseline.Remote.Digest()
	if before != after || beforeRemote != afterRemote {
		t.Fatal("replan adopted partial result as baseline")
	}
	next, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil || len(next.Plan.Conflicts) != 1 || next.Plan.Conflicts[0].Reason != "both_sides_changed" {
		t.Fatal("later conflict concealed", next, err)
	}
	history, err := f.engine.RecoveryHistory(t.Context(), f.saved.ID)
	if err != nil || len(history[0].Files) != 1 {
		t.Fatal("archived recovery unavailable", err)
	}
	export, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	filename, err := f.engine.ExportRecovery(t.Context(), f.saved.ID, history[0].ID, history[0].Files[0].ID, export)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(export, filename))
	if err != nil || string(raw) != "old" {
		t.Fatal("archive lost old version", err)
	}
}
func TestSyncRecoveryForeignUserRejected(t *testing.T) {
	f := newEngineFixture(t, true, nil)
	writeEngineFile(t, f.local, "file", "x")
	engineRound(t, f)
	bob, err := syncclient.NewRemote(f.saved.Binding.Server, "client-fixture-bob", false)
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	engine := syncclient.Engine{State: f.engine.State, Remote: bob}
	if _, err = engine.RecoveryHistory(t.Context(), f.saved.ID); !errors.Is(err, syncclient.ErrBinding) {
		t.Fatal("foreign user read history", err)
	}
	if _, err = engine.ExportRecovery(t.Context(), f.saved.ID, strings.Repeat("a", 32), strings.Repeat("b", 32), t.TempDir()); !errors.Is(err, syncclient.ErrBinding) {
		t.Fatal("foreign user attempted export", err)
	}
}

func TestSyncRecoveryProtocolReviewsResolvesAndExportsArchivedCopy(t *testing.T) {
	var fail atomic.Bool
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail.Load() && strings.HasSuffix(r.URL.Path, "/sync/apply") {
				response := httptest.NewRecorder()
				next.ServeHTTP(response, r)
				w.Header().Set("X-Agentbox-Server-ID", response.Header().Get("X-Agentbox-Server-ID"))
				w.WriteHeader(503)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	writeEngineFile(t, f.local, "file", "original")
	writeEngineFile(t, f.remote, "file", "original")
	engineRound(t, f)
	writeEngineFile(t, f.local, "file", "new")
	fail.Store(true)
	preview, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest); err == nil {
		t.Fatal("expected interrupted fixture")
	}
	input, commands := io.Pipe()
	output, responses := io.Pipe()
	done := make(chan error, 1)
	go func() { err := syncclient.RunDesktop(input, responses); responses.CloseWithError(err); done <- err }()
	t.Cleanup(func() {
		commands.Close()
		output.Close()
		input.Close()
		responses.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("sidecar did not terminate")
		}
	})
	encoder, decoder := json.NewEncoder(commands), json.NewDecoder(output)
	var ready map[string]any
	if err = decoder.Decode(&ready); err != nil {
		t.Fatal(err)
	}
	call := func(kind string, fields map[string]any) map[string]json.RawMessage {
		t.Helper()
		request := map[string]any{"version": 1, "id": "recovery-command", "type": kind, "server": f.saved.Binding.Server, "user": "alice", "token": "client-fixture-alice", "state_dir": f.stateDir, "binding_id": f.saved.ID}
		for k, v := range fields {
			request[k] = v
		}
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
		var result map[string]json.RawMessage
		if err := decoder.Decode(&result); err != nil {
			t.Fatal(err)
		}
		if string(result["type"]) == `"error"` {
			t.Fatalf("%s: %s", kind, result["error"])
		}
		raw, _ := json.Marshal(result)
		if strings.Contains(string(raw), "client-fixture-alice") || strings.Contains(string(raw), "lease_token") {
			t.Fatal("credential escaped into IPC response")
		}
		return result
	}
	result := call("sync_review", nil)
	var review syncclient.PendingReview
	if err = json.Unmarshal(result["review"], &review); err != nil || !review.CanFinish {
		t.Fatal(review, err)
	}
	result = call("sync_resolve", map[string]any{"confirmation": review.Digest, "action": "finish"})
	if string(result["type"]) != `"sync_resolved"` {
		t.Fatal(result)
	}
	result = call("sync_history", nil)
	var history []syncclient.RecoveryBatch
	if err = json.Unmarshal(result["history"], &history); err != nil || len(history[0].Files) != 1 {
		t.Fatal(history, err)
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result = call("sync_export", map[string]any{"batch_id": history[0].ID, "operation_id": history[0].Files[0].ID, "export_directory": directory})
	var name string
	if err = json.Unmarshal(result["filename"], &name); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil || string(raw) != "original" {
		t.Fatal("IPC export lost original bytes", err)
	}
}
