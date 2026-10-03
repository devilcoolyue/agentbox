package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"agentbox/internal/syncclient"
	"agentbox/internal/syncproto"
)

func orphanEngine(t *testing.T, f retirementFixture, wrap func(http.Handler) http.Handler) (*syncclient.Engine, syncclient.OrphanRecoveryScope) {
	t.Helper()
	handler := f.handler
	if wrap != nil {
		handler = wrap(handler)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	remote, err := syncclient.NewRemote(server.URL, "client-fixture-alice", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, err := syncclient.OpenState(filepath.Join(private, "empty-state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	device, err := state.Device()
	if err != nil || device == f.status.Device {
		t.Fatal("fixture must use new local device", device, err)
	}
	return &syncclient.Engine{Remote: remote, State: state}, syncclient.OrphanRecoveryScope{ServerID: f.identity, User: "alice", Workspace: f.sess.ID, Device: f.status.Device, OperationID: f.mutation.ID}
}

func TestOrphanRecoveryWithoutLocalHistoryOrSyncAvailability(t *testing.T) {
	f := newRetirementFixture(t, true)
	e, scope := orphanEngine(t, f, nil)
	identity, err := e.Remote.Identity(t.Context())
	if err != nil || identity.Features["sync"] != 0 || identity.Features["sync_recovery_inspect"] != 1 {
		t.Fatal(identity, err)
	}
	page, err := e.ListOrphanRecovery(t.Context(), scope, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].Status.Device != scope.Device || page.NextCursor != "" {
		t.Fatal(page, err)
	}
	review, err := e.ReviewOrphanRecovery(t.Context(), scope)
	if err != nil || !review.CanRetire || review.Comparison != "matches_after" || review.RecoveryState != "available" {
		t.Fatal(review, err)
	}
	destination, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	filename, err := e.ExportOrphanRecovery(t.Context(), scope, destination)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, filename)); err != nil || string(data) != "old" {
		t.Fatal(string(data), err)
	}
	if _, err = e.ExportOrphanRecovery(t.Context(), scope, destination); err == nil {
		t.Fatal("export overwrote existing file")
	}
	// Later edits do not revoke a historical applied receipt. They do invalidate
	// an unstarted confirmation, and a new review explains the difference.
	current := filepath.Join(f.s.workspaceDir(f.sess), "file")
	if err = os.WriteFile(current, []byte("user's later edit"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = e.RetireOrphanRecovery(t.Context(), scope, review.Digest); !errors.Is(err, syncproto.ErrChanged) {
		t.Fatal("stale review accepted", err)
	}
	review, err = e.ReviewOrphanRecovery(t.Context(), scope)
	if err != nil || review.Comparison != "changed" || !review.CanRetire || review.Status.Operation.Status != "applied" {
		t.Fatal(review, err)
	}
	retired, err := e.RetireOrphanRecovery(t.Context(), scope, review.Digest)
	if err != nil || retired.Status.Retirement != "retired" || retired.CanRetire {
		t.Fatal(retired, err)
	}
	if raw, _ := os.ReadFile(current); string(raw) != "user's later edit" {
		t.Fatal("cleanup changed current file")
	}
	if _, err = e.ExportOrphanRecovery(t.Context(), scope, t.TempDir()); err == nil {
		t.Fatal("retired recovery exported")
	}
	if storage := f.storage(t); storage.ActiveOperations != 0 || storage.RetainedReceipts != 1 || storage.RecoveryBytes != 0 {
		t.Fatal(storage)
	}
	lease, err := f.s.syncLeases().Acquire(f.sess.ID, f.mutation.Project, ".", f.mutation.Device)
	if err != nil {
		t.Fatal(err)
	}
	f.mutation.Generation = lease.Generation
	w := mutationRequest(f.handler, f.mutation, lease, strings.NewReader("new\r\n\x00"), "alice")
	var replay syncproto.MutationResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &replay) != nil || !replay.Replayed || replay.Recovery || replay.Status != "applied" {
		t.Fatal("orphan cleanup lost original replay fence", w.Code, w.Body.String())
	}
	if raw, _ := os.ReadFile(current); string(raw) != "user's later edit" {
		t.Fatal("retired operation was replayed")
	}
}

func TestOrphanRecoveryLostResponseRestartAndCurrentEditRetry(t *testing.T) {
	f := newRetirementFixture(t, true)
	var lost atomic.Bool
	lost.Store(true)
	e, scope := orphanEngine(t, f, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" && strings.Contains(r.URL.Path, "/recovery-operations/") && lost.Swap(false) {
				response := httptest.NewRecorder()
				next.ServeHTTP(response, r)
				if response.Code != 200 {
					t.Error(response.Code, response.Body.String())
				}
				w.Header().Set("X-Agentbox-Server-ID", f.identity)
				w.WriteHeader(503)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	review, err := e.ReviewOrphanRecovery(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.RetireOrphanRecovery(t.Context(), scope, review.Digest); err == nil {
		t.Fatal("lost response reported success")
	}
	f.s.clientLeases = syncproto.NewLeases()
	f.s.forgetClientInventory(f.sess.ID)
	if err = os.WriteFile(filepath.Join(f.s.workspaceDir(f.sess), "file"), []byte("edited after loss"), 0644); err != nil {
		t.Fatal(err)
	}
	// Fresh native state and remote transport prove retry does not depend on the
	// original process, device ID, local binding, or an in-memory authorization.
	restarted, scope := orphanEngine(t, f, nil)
	retired, err := restarted.RetireOrphanRecovery(t.Context(), scope, review.Digest)
	if err != nil || retired.Status.Retirement != "retired" {
		t.Fatal(retired, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(f.s.workspaceDir(f.sess), "file")); string(raw) != "edited after loss" {
		t.Fatal("retry wrote current file")
	}
}

func TestOrphanRecoveryUncertainBytesRemainExportableWithoutClaimingSuccess(t *testing.T) {
	f := newRetirementFixture(t, true)
	operation, err := safefs.Open(filepath.Join(f.s.sessionDir(f.sess), "client-sync", f.mutation.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer operation.Close()
	record, err := loadClientMutationRecord(operation, f.mutation.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Result.Status = "uncertain"
	record.Result.Recovery = false
	if err = saveClientMutation(operation, record); err != nil {
		t.Fatal(err)
	}
	e, scope := orphanEngine(t, f, nil)
	review, err := e.ReviewOrphanRecovery(t.Context(), scope)
	if err != nil || review.Status.Operation.Status != "uncertain" || review.CanRetire || review.Comparison != "matches_after" || review.RecoveryState != "available" {
		t.Fatal(review, err)
	}
	dest, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name, err := e.ExportOrphanRecovery(t.Context(), scope, dest)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dest, name)); string(raw) != "old" {
		t.Fatal("uncertain recovery lost")
	}
	if _, err = e.RetireOrphanRecovery(t.Context(), scope, review.Digest); !errors.Is(err, syncproto.ErrChanged) {
		t.Fatal("uncertain operation declared completed", err)
	}
	record, err = loadClientMutationRecord(operation, f.mutation.ID)
	if err != nil || record.Result.Status != "uncertain" || record.Retirement != "" {
		t.Fatal(record, err)
	}
}

func TestOrphanRecoveryPagePreservesUnknownAndDeviceScope(t *testing.T) {
	f := newRetirementFixture(t, false)
	journal := filepath.Join(f.s.sessionDir(f.sess), "client-sync")
	for n := 1; n <= 106; n++ {
		id := fmt.Sprintf("%032x", n)
		if err := os.Mkdir(filepath.Join(journal, id), 0700); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			continue
		}
		if n == 2 {
			if err := os.WriteFile(filepath.Join(journal, id, "record.json"), []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		mutation := f.mutation
		mutation.ID = id
		mutation.Generation = ""
		if n%2 == 0 {
			mutation.Device = "other-device"
		}
		copy := mutation
		copy.Generation = "validate"
		digest, err := copy.Digest()
		if err != nil {
			t.Fatal(err)
		}
		record := clientMutationRecord{Digest: digest, Request: mutation, Result: syncproto.MutationResult{ID: id, Status: "applied"}}
		raw, _ := json.Marshal(record)
		if err = os.WriteFile(filepath.Join(journal, id, "record.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	e, scope := orphanEngine(t, f, nil)
	scope.Device = ""
	seen := map[string]bool{}
	cursor := ""
	unknown := 0
	pages := 0
	for {
		page, err := e.ListOrphanRecovery(t.Context(), scope, cursor)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate page entry", item.ID)
			}
			seen[item.ID] = true
			if item.Status == nil {
				unknown++
				if item.Issue == "" {
					t.Fatal("unknown claimed normal")
				}
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		wrong := scope
		wrong.Device = "other-device"
		if _, err = e.ListOrphanRecovery(t.Context(), wrong, cursor); err == nil {
			t.Fatal("cursor crossed device scope")
		}
	}
	if len(seen) != 107 || unknown != 2 || pages != 3 {
		t.Fatal(len(seen), unknown, pages)
	}
	scope.Device = "other-device"
	cursor = ""
	count := 0
	for {
		page, err := e.ListOrphanRecovery(t.Context(), scope, cursor)
		if err != nil {
			t.Fatal(err)
		}
		count += len(page.Items)
		for _, item := range page.Items {
			if item.Status == nil || item.Status.Device != scope.Device {
				t.Fatal(item)
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if count != 52 {
		t.Fatal(count)
	}
	// An unreadable journal has unknown scope. Filtering must not turn that I/O
	// failure into a successful empty/partial inventory of a selected device.
	unsafeID := strings.Repeat("0", 31) + "0"
	if err := os.Mkdir(filepath.Join(journal, unsafeID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(journal, f.mutation.ID, "record.json"), filepath.Join(journal, unsafeID, "record.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ListOrphanRecovery(t.Context(), scope, ""); err == nil {
		t.Fatal("device filter hid unreadable journal")
	}
}

func TestOrphanRecoveryScopeLeaseAndUnsafeOldBytes(t *testing.T) {
	for _, kind := range []string{"wrong_instance", "missing_instance", "wrong_owner", "wrong_device", "lease", "corrupt_before", "missing_before", "before_symlink", "before_hardlink", "current_symlink", "project_missing", "project_changed"} {
		t.Run(kind, func(t *testing.T) {
			f := newRetirementFixture(t, true)
			endpoint := "recovery-operations/" + f.mutation.ID
			user, identity, device := "alice", f.identity, f.status.Device
			name := filepath.Join(f.s.sessionDir(f.sess), "client-sync", f.mutation.ID, "before")
			switch kind {
			case "wrong_instance":
				identity = strings.Repeat("f", 64)
			case "missing_instance":
				identity = ""
			case "wrong_owner":
				user = "bob"
			case "wrong_device":
				device = "other-device"
			case "lease":
				if _, err := f.s.syncLeases().Acquire(f.sess.ID, "other-project", "other", "other-device"); err != nil {
					t.Fatal(err)
				}
			case "corrupt_before":
				if err := os.WriteFile(name, []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing_before":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "before_symlink":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(f.s.workspaceDir(f.sess), "file"), name); err != nil {
					t.Fatal(err)
				}
			case "before_hardlink":
				if err := os.Link(name, name+"-link"); err != nil {
					t.Fatal(err)
				}
			case "current_symlink":
				name := filepath.Join(f.s.workspaceDir(f.sess), "file")
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/outside-must-not-be-read", name); err != nil {
					t.Fatal(err)
				}
			case "project_missing":
				if err := f.s.store.DeleteClientProject(f.sess.ID, f.mutation.Project, f.mutation.Revision); err != nil {
					t.Fatal(err)
				}
			case "project_changed":
				p, err := f.s.store.ClientProject(f.sess.ID, f.mutation.Project)
				if err != nil {
					t.Fatal(err)
				}
				p.Name = "renamed"
				if _, err = f.s.store.UpdateClientProject(p); err != nil {
					t.Fatal(err)
				}
			}
			w := f.request("GET", endpoint+"?device="+url.QueryEscape(device), nil, user, identity)
			if strings.HasPrefix(kind, "wrong_") || kind == "missing_instance" {
				if w.Code == 200 {
					t.Fatal("scope check bypassed", kind)
				}
				return
			}
			var review syncproto.RecoveryOperationReview
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil || review.Validate(f.identity, f.sess.ID, f.status.Device, f.mutation.ID) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			expectAllowed := kind == "project_missing" || kind == "project_changed" || kind == "current_symlink"
			if review.CanRetire != expectAllowed {
				t.Fatal(kind, review)
			}
			if kind == "project_missing" && review.Comparison != "project_missing" || kind == "project_changed" && review.Comparison != "project_changed" || kind == "current_symlink" && review.Comparison != "unavailable" {
				t.Fatal(review)
			}
			w = f.request("POST", endpoint+"/retire", syncproto.RecoveryInspectRetireRequest{Device: device, Confirmation: review.Digest}, user, identity)
			if (w.Code == 200) != expectAllowed {
				t.Fatal(kind, w.Code, w.Body.String())
			}
		})
	}
}

func TestOrphanRecoveryInterruptedRetiringIntentResumes(t *testing.T) {
	for _, stage := range []string{"after_intent", "before_unlink", "after_unlink", "after_unlink_sync"} {
		t.Run(stage, func(t *testing.T) {
			f := newRetirementFixture(t, true)
			e, scope := orphanEngine(t, f, nil)
			review, err := e.ReviewOrphanRecovery(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			operation, err := safefs.Open(filepath.Join(f.s.sessionDir(f.sess), "client-sync", f.mutation.ID))
			if err != nil {
				t.Fatal(err)
			}
			defer operation.Close()
			record, err := loadClientMutationRecord(operation, f.mutation.ID)
			if err != nil {
				t.Fatal(err)
			}
			record.InspectionConfirmation = review.Digest
			err = retireClientOperation(t.Context(), operation, &record, func(where string) error {
				if where == stage {
					return context.Canceled
				}
				return nil
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			f.s.forgetClientInventory(f.sess.ID)
			if err = os.WriteFile(filepath.Join(f.s.workspaceDir(f.sess), "file"), []byte("edit during restart"), 0644); err != nil {
				t.Fatal(err)
			}
			fresh, scope := orphanEngine(t, f, nil)
			retired, err := fresh.RetireOrphanRecovery(t.Context(), scope, review.Digest)
			if err != nil || retired.Status.Retirement != "retired" {
				t.Fatal(retired, err)
			}
		})
	}
}

func TestOrphanRecoveryRefusesExistingPendingReference(t *testing.T) {
	var fail atomic.Bool
	f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/sync/apply") && fail.Swap(false) {
				response := httptest.NewRecorder()
				next.ServeHTTP(response, r)
				if response.Code != 200 {
					t.Error(response.Code, response.Body.String())
				}
				w.Header().Set("X-Agentbox-Server-ID", response.Header().Get("X-Agentbox-Server-ID"))
				w.WriteHeader(503)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	writeEngineFile(t, f.local, "file", "old")
	engineRound(t, f)
	writeEngineFile(t, f.local, "file", "new")
	preview, err := f.engine.Preview(t.Context(), f.saved.ID, syncclient.Automatic)
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if _, err = f.engine.Apply(t.Context(), f.saved.ID, preview, preview.Plan.Digest); err == nil {
		t.Fatal("lost mutation response was reported successful")
	}
	saved, err := f.engine.State.Load(f.saved.ID)
	if err != nil || saved.Pending == nil || len(saved.Pending.Operations) != 1 {
		t.Fatal(saved, err)
	}
	device, err := f.engine.State.Device()
	if err != nil {
		t.Fatal(err)
	}
	scope := syncclient.OrphanRecoveryScope{ServerID: saved.Binding.ServerID, User: saved.Binding.User, Workspace: saved.Binding.Workspace, Device: device, OperationID: saved.Pending.Operations[0].ID}
	review, err := f.engine.ReviewOrphanRecovery(t.Context(), scope)
	if err != nil || !review.LocalPending || review.CanRetire || review.Status.Operation.Status != "applied" {
		t.Fatal(review, err)
	}
	if _, err = f.engine.RetireOrphanRecovery(t.Context(), scope, review.Digest); !errors.Is(err, syncclient.ErrPending) {
		t.Fatal("discarded pending batch recovery", err)
	}
	if _, err = os.Stat(filepath.Join(f.server.sessionDir(f.session), "client-sync", scope.OperationID, "before")); err != nil {
		t.Fatal("pending recovery removed", err)
	}
	aliasURL := strings.Replace(saved.Binding.Server, "127.0.0.1", "localhost", 1)
	if aliasURL == saved.Binding.Server {
		t.Fatal("fixture did not produce a second URL")
	}
	aliasRemote, err := syncclient.NewRemote(aliasURL, "client-fixture-alice", false)
	if err != nil {
		t.Fatal(err)
	}
	defer aliasRemote.Close()
	aliasEngine := &syncclient.Engine{State: f.engine.State, Remote: aliasRemote}
	aliasReview, err := aliasEngine.ReviewOrphanRecovery(t.Context(), scope)
	if err != nil || !aliasReview.LocalPending || aliasReview.CanRetire {
		t.Fatal("same instance URL alias hid pending reference", aliasReview, err)
	}
	if _, err = aliasEngine.RetireOrphanRecovery(t.Context(), scope, review.Digest); !errors.Is(err, syncclient.ErrPending) {
		t.Fatal("same instance alias bypassed pending protection", err)
	}
}

// seedOrphanRecoveryFixture is shared with the explicit desktop native smoke.
// It creates a genuine server receipt and before bytes, without local history.
// Call after existing full-tree assertions: this adds a dedicated project root.
func seedOrphanRecoveryFixture(t *testing.T, f *engineFixture) syncproto.OperationStatus {
	t.Helper()
	name := "orphan-recovery-fixture"
	writeEngineFile(t, f.remote, name+"/old.txt", "orphan-recovery-before\r\n中文\x00")
	p, err := f.server.store.CreateClientProject(store.ClientProject{SessionID: f.session.ID, Name: "Orphan recovery fixture", Path: name})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.server.syncLeases().Acquire(f.session.ID, p.ID, p.Path, "orphan-fixture-device")
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := syncproto.ParseRules("")
	m := syncproto.Mutation{Version: 1, ID: strings.Repeat("f", 32), Project: p.ID, Revision: p.Revision, RulesHash: rules.Hash(), Device: lease.Device, Generation: lease.Generation, Path: "old.txt", Kind: "replace", Before: mutationEntry("orphan-recovery-before\r\n中文\x00"), After: mutationEntry("orphan latest\r\n")}
	w := mutationRequest(f.server.Handler(), m, lease, strings.NewReader("orphan latest\r\n"), "alice")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err = f.server.syncLeases().Release(f.session.ID, p.ID, p.Path, lease.Device, lease.Token, lease.Generation); err != nil {
		t.Fatal(err)
	}
	id, err := f.server.clientIdentity()
	if err != nil {
		t.Fatal(err)
	}
	root, err := safefs.Open(filepath.Join(f.server.sessionDir(f.session), "client-sync", m.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	record, err := loadClientMutationRecord(root, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	return clientOperationStatus(record, id, f.session.ID)
}
