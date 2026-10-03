package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"agentbox/internal/syncclient"
	"agentbox/internal/syncproto"
)

func remoteCleanupFixture(t *testing.T, wrap func(http.Handler) http.Handler) (*engineFixture, syncclient.SavedBinding, syncclient.Batch) {
	t.Helper()
	f := newEngineFixture(t, true, wrap)
	writeEngineFile(t, f.local, "replace", "original\r\n\x00")
	engineRound(t, f)
	writeEngineFile(t, f.local, "replace", "latest")
	writeEngineFile(t, f.local, "create", "new file")
	saved := engineRound(t, f)
	page, err := f.engine.RecoveryHistoryPage(t.Context(), saved.ID, "")
	if err != nil || len(page.History) == 0 {
		t.Fatal(page, err)
	}
	batch, err := f.engine.State.History(saved.ID, page.History[0].ID)
	if err != nil || len(batch.Operations) != 2 {
		t.Fatal(batch, err)
	}
	return f, saved, batch
}

func TestSyncRemoteCleanupPreservesCurrentFilesBaselineAndAudit(t *testing.T) {
	f, saved, batch := remoteCleanupFixture(t, nil)
	storage, err := f.engine.Remote.RecoveryStorage(t.Context(), f.session.ID)
	if err != nil || storage.ActiveOperations != 3 || storage.RetainedReceipts != 0 || storage.RecoveryBytes != int64(len("original\r\n\x00")) {
		t.Fatal("capacity does not describe the unretired journal", storage, err)
	}
	review, err := f.engine.ReviewRemoteCleanup(t.Context(), saved.ID, batch.ID, saved.Revision)
	if err != nil || review.BindingID != saved.ID || review.BatchID != batch.ID || review.Revision != saved.Revision || !syncproto.ValidHash(review.Digest) || len(review.Items) != 2 {
		t.Fatal(review, err)
	}
	var bytes int64
	for _, item := range review.Items {
		bytes += item.Bytes
	}
	if bytes != int64(len("original\r\n\x00")) {
		t.Fatal("review lost old bytes or omitted zero-byte receipt", review)
	}
	cleaned, err := f.engine.CleanupRemoteRecovery(t.Context(), saved.ID, batch.ID, saved.Revision, review.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.Revision <= saved.Revision || cleaned.Pending != nil || !reflect.DeepEqual(cleaned.Baseline, saved.Baseline) {
		t.Fatal("cleanup changed baseline or failed to persist progress", cleaned)
	}
	storage, err = f.engine.Remote.RecoveryStorage(t.Context(), f.session.ID)
	if err != nil || storage.ActiveOperations != 1 || storage.RetainedReceipts != 2 || storage.RecoveryBytes != 0 || storage.MetadataBytes <= 0 {
		t.Fatal("cleanup failed to release capacity or retain receipts", storage, err)
	}
	archived, err := f.engine.State.History(saved.ID, batch.ID)
	if err != nil || !reflect.DeepEqual(archived.Plan, batch.Plan) || len(archived.Operations) != len(batch.Operations) {
		t.Fatal("cleanup removed historical audit", archived, err)
	}
	for i, op := range archived.Operations {
		if op.ID != batch.Operations[i].ID || op.Status != batch.Operations[i].Status || op.RemoteRetirement != "retired" {
			t.Fatal("cleanup changed operation acknowledgement", op)
		}
		receipt, err := f.engine.Remote.Operation(t.Context(), f.session.ID, op.ID)
		if err != nil || receipt.Operation.Status != "applied" || receipt.Operation.Recovery || receipt.Retirement != "retired" {
			t.Fatal("missing replay fence", receipt, err)
		}
		if _, err = f.engine.ExportRecovery(t.Context(), saved.ID, batch.ID, op.ID, t.TempDir()); err == nil {
			t.Fatal("retired copy exported", op.ID)
		}
	}
	for name, want := range map[string]string{"replace": "latest", "create": "new file"} {
		for _, root := range []string{f.local, f.remote} {
			data, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || string(data) != want {
				t.Fatal("cleanup touched current files", root, name, err)
			}
		}
	}
	preview, err := f.engine.Preview(t.Context(), saved.ID, syncclient.Automatic)
	if err != nil || len(preview.Plan.Operations) != 0 || len(preview.Plan.Conflicts) != 0 {
		t.Fatal("cleanup changed next synchronization", preview, err)
	}
}

func TestSyncRemoteCleanupRejectsStaleRevisionAndConfirmation(t *testing.T) {
	var retired atomic.Int32
	f, saved, batch := remoteCleanupFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/retire") {
				retired.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	if _, err := f.engine.ReviewRemoteCleanup(t.Context(), saved.ID, batch.ID, saved.Revision-1); !errors.Is(err, syncclient.ErrStateChanged) {
		t.Fatal("stale review accepted", err)
	}
	review, err := f.engine.ReviewRemoteCleanup(t.Context(), saved.ID, batch.ID, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		revision int64
		digest   string
	}{{saved.Revision - 1, review.Digest}, {saved.Revision, strings.Repeat("0", 64)}, {saved.Revision, ""}} {
		if _, err = f.engine.CleanupRemoteRecovery(t.Context(), saved.ID, batch.ID, check.revision, check.digest); err == nil {
			t.Fatal("stale or absent approval accepted")
		}
	}
	if retired.Load() != 0 {
		t.Fatal("rejected cleanup sent retirement requests")
	}
	current, err := f.engine.State.Load(saved.ID)
	if err != nil || current.Revision != saved.Revision || !reflect.DeepEqual(current.Baseline, saved.Baseline) {
		t.Fatal("rejected cleanup changed state", err)
	}
}

func TestSyncRemoteCleanupReconciledFinishKeepsStartedAudit(t *testing.T) {
	var interrupt atomic.Bool
	var applyCalls atomic.Int32
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/sync/apply") {
				applyCalls.Add(1)
				if interrupt.Load() {
					response := httptest.NewRecorder()
					next.ServeHTTP(response, r)
					if response.Code != http.StatusOK {
						t.Error("publication fixture failed", response.Code, response.Body.String())
					}
					w.Header().Set("X-Agentbox-Server-ID", response.Header().Get("X-Agentbox-Server-ID"))
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	writeEngineFile(t, f.local, "file", "before")
	engineRound(t, f)
	writeEngineFile(t, f.local, "file", "after")
	preview, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	interrupt.Store(true)
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest); err == nil {
		t.Fatal("fixture did not interrupt the batch")
	}
	pending, err := f.engine.ReviewPending(t.Context(), f.saved.ID)
	if err != nil || !pending.CanFinish {
		t.Fatal(pending, err)
	}
	saved, err := f.engine.ResolvePending(t.Context(), f.saved.ID, pending.Digest, "finish")
	if err != nil || saved.Pending != nil || saved.Baseline == nil {
		t.Fatal(saved, err)
	}
	batch, err := f.engine.State.History(saved.ID, pending.BatchID)
	if err != nil || len(batch.Operations) != 1 || batch.Operations[0].Status != "started" || batch.Resolution == nil || batch.Resolution.Action != "finish" {
		t.Fatal("finish rewrote original acknowledgment", batch, err)
	}
	review, err := f.engine.ReviewRemoteCleanup(t.Context(), saved.ID, batch.ID, saved.Revision)
	if err != nil || len(review.Items) != 1 || review.Items[0].Bytes != int64(len("before")) {
		t.Fatal("confirmed finish excluded from retirement", review, err)
	}
	cleaned, err := f.engine.CleanupRemoteRecovery(t.Context(), saved.ID, batch.ID, saved.Revision, review.Digest)
	if err != nil || !reflect.DeepEqual(cleaned.Baseline, saved.Baseline) {
		t.Fatal("retirement changed confirmed baseline", err)
	}
	stored, err := f.engine.State.History(saved.ID, batch.ID)
	if err != nil || stored.Operations[0].Status != "started" || stored.Operations[0].RemoteRetirement != "retired" || !reflect.DeepEqual(stored.Resolution, batch.Resolution) {
		t.Fatal("retirement lost original finish audit", stored, err)
	}
	if applyCalls.Load() != 2 {
		t.Fatal("reconciliation or cleanup replayed the mutation", applyCalls.Load())
	}
}

func TestSyncRemoteCleanupRefusesPendingReplanAndAbandonedBatch(t *testing.T) {
	for _, action := range []string{"pending", "replan", "abandoned"} {
		t.Run(action, func(t *testing.T) {
			f, _ := lostEngineResponse(t, map[string]string{"a": "first", "b": "second"})
			saved, err := f.engine.State.Load(f.saved.ID)
			if err != nil {
				t.Fatal(err)
			}
			batchID := saved.Pending.ID
			switch action {
			case "replan":
				review, err := f.engine.ReviewPending(t.Context(), saved.ID)
				if err != nil {
					t.Fatal(err)
				}
				saved, err = f.engine.ResolvePending(t.Context(), saved.ID, review.Digest, "replan")
				if err != nil {
					t.Fatal(err)
				}
			case "abandoned":
				review, err := f.engine.ReviewAbandon(t.Context(), saved.ID)
				if err != nil {
					t.Fatal(err)
				}
				saved, err = f.engine.AbandonPending(t.Context(), saved.ID, review.Digest)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = f.engine.ReviewRemoteCleanup(t.Context(), saved.ID, batchID, saved.Revision); err == nil {
				t.Fatal("unverified batch offered for cleanup")
			}
			if _, err = f.engine.CleanupRemoteRecovery(t.Context(), saved.ID, batchID, saved.Revision, strings.Repeat("0", 64)); err == nil {
				t.Fatal("unverified batch cleaned")
			}
			current, err := f.engine.State.Load(saved.ID)
			if err != nil || !reflect.DeepEqual(current, saved) {
				t.Fatal("refusal changed saved state", err)
			}
		})
	}
}

func TestSyncRemoteCleanupRejectsForeignUserReplacementAndOldCapability(t *testing.T) {
	for _, mode := range []string{"foreign", "replacement", "old-capability"} {
		t.Run(mode, func(t *testing.T) {
			var changed atomic.Bool
			var references atomic.Int32
			f, saved, batch := remoteCleanupFixture(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if changed.Load() && strings.Contains(r.URL.Path, "/sync/") {
						if mode != "old-capability" || strings.HasSuffix(r.URL.Path, "/storage") || strings.HasSuffix(r.URL.Path, "/retire") {
							references.Add(1)
						}
					}
					if changed.Load() && r.URL.Path == "/api/clients/capabilities" && mode != "foreign" {
						response := httptest.NewRecorder()
						next.ServeHTTP(response, r)
						var identity syncproto.ServerIdentity
						if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &identity) != nil {
							t.Error("invalid fixture identity")
							w.WriteHeader(http.StatusInternalServerError)
							return
						}
						if mode == "replacement" {
							identity.ServerID = syncproto.HashBytes([]byte("replacement instance"))
						} else {
							delete(identity.Features, "sync_recovery_gc")
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(identity)
						return
					}
					next.ServeHTTP(w, r)
				})
			})
			review, err := f.engine.ReviewRemoteCleanup(t.Context(), saved.ID, batch.ID, saved.Revision)
			if err != nil {
				t.Fatal(err)
			}
			engine := f.engine
			if mode == "foreign" {
				bob, err := syncclient.NewRemote(saved.Binding.Server, "client-fixture-bob", false)
				if err != nil {
					t.Fatal(err)
				}
				defer bob.Close()
				engine = &syncclient.Engine{State: f.engine.State, Remote: bob}
			}
			changed.Store(true)
			if _, err = engine.ReviewRemoteCleanup(t.Context(), saved.ID, batch.ID, saved.Revision); err == nil {
				t.Fatal("unsupported identity or capability permitted review")
			}
			if _, err = engine.CleanupRemoteRecovery(t.Context(), saved.ID, batch.ID, saved.Revision, review.Digest); err == nil {
				t.Fatal("unsupported identity or capability permitted cleanup")
			}
			if references.Load() != 0 {
				t.Fatal("cleanup disclosed references or called unsupported routes", references.Load())
			}
		})
	}
}

func TestSyncRemoteCleanupLostResponseResumesAfterRestart(t *testing.T) {
	var lose atomic.Bool
	lose.Store(true)
	var retireCalls atomic.Int32
	f, saved, batch := remoteCleanupFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/retire") {
				retireCalls.Add(1)
				if lose.Swap(false) {
					response := httptest.NewRecorder()
					next.ServeHTTP(response, r)
					if response.Code != http.StatusOK {
						t.Error("retirement fixture failed", response.Code, response.Body.String())
					}
					w.Header().Set("X-Agentbox-Server-ID", response.Header().Get("X-Agentbox-Server-ID"))
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	review, err := f.engine.ReviewRemoteCleanup(t.Context(), saved.ID, batch.ID, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.CleanupRemoteRecovery(t.Context(), saved.ID, batch.ID, saved.Revision, review.Digest); err == nil {
		t.Fatal("lost response treated as fully acknowledged")
	}
	if retireCalls.Load() != 1 {
		t.Fatal("failed cleanup retried automatically or continued", retireCalls.Load())
	}
	if err = f.engine.State.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := syncclient.OpenState(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f.engine.State = reopened
	interrupted, err := reopened.Load(saved.ID)
	if err != nil || !reflect.DeepEqual(interrupted.Baseline, saved.Baseline) {
		t.Fatal("cleanup interruption altered baseline", err)
	}
	stored, err := reopened.History(saved.ID, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	var retiring int
	for _, op := range stored.Operations {
		if op.RemoteRetirement == "retiring" {
			retiring++
		}
	}
	if retiring == 0 {
		t.Fatal("restart lost durable cleanup intent", stored.Operations)
	}
	review, err = f.engine.ReviewRemoteCleanup(t.Context(), saved.ID, batch.ID, interrupted.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.CleanupRemoteRecovery(t.Context(), saved.ID, batch.ID, interrupted.Revision, review.Digest); err != nil {
		t.Fatal("explicit retry failed to finish retirement", err)
	}
	stored, err = reopened.History(saved.ID, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range stored.Operations {
		if op.RemoteRetirement != "retired" {
			t.Fatal("retry left unfinished audit", op)
		}
	}
}

func TestSyncRetiredReceiptCannotFinishPendingOrExport(t *testing.T) {
	var interrupted atomic.Bool
	var recoveryReads atomic.Int32
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if interrupted.Load() && strings.Contains(r.URL.Path, "/sync/operations/") && strings.HasSuffix(r.URL.Path, "/recovery") {
				recoveryReads.Add(1)
			}
			if interrupted.Load() && (strings.HasSuffix(r.URL.Path, "/sync/apply") || strings.Contains(r.URL.Path, "/sync/operations/") && !strings.HasSuffix(r.URL.Path, "/recovery")) {
				response := httptest.NewRecorder()
				next.ServeHTTP(response, r)
				for name, values := range response.Header() {
					w.Header()[name] = values
				}
				if strings.HasSuffix(r.URL.Path, "/sync/apply") {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				var receipt syncproto.OperationStatus
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &receipt) != nil {
					t.Error("invalid fixture operation")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				receipt.Retirement = "retired"
				receipt.Operation.Recovery = false
				_ = json.NewEncoder(w).Encode(receipt)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	writeEngineFile(t, f.local, "file", "before")
	engineRound(t, f)
	writeEngineFile(t, f.local, "file", "after")
	preview, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	interrupted.Store(true)
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest); err == nil {
		t.Fatal("fixture did not interrupt the batch")
	}
	saved, err := f.engine.State.Load(f.saved.ID)
	if err != nil || saved.Pending == nil {
		t.Fatal(saved, err)
	}
	review, err := f.engine.ReviewPending(t.Context(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if review.CanFinish {
		t.Fatal("retired receipt counted as finish evidence", review)
	}
	if _, err = f.engine.ResolvePending(t.Context(), saved.ID, review.Digest, "finish"); err == nil {
		t.Fatal("retired receipt advanced baseline")
	}
	if _, err = f.engine.ExportRecovery(t.Context(), saved.ID, saved.Pending.ID, saved.Pending.Operations[0].ID, t.TempDir()); err == nil {
		t.Fatal("retired receipt permitted recovery export")
	}
	if recoveryReads.Load() != 0 {
		t.Fatal("retired receipt initiated recovery download")
	}
	current, err := f.engine.State.Load(saved.ID)
	if err != nil || !reflect.DeepEqual(current, saved) {
		t.Fatal("retired receipt changed pending state", err)
	}
}
