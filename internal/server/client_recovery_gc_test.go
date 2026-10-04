package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/safefs"
	"agentbox/internal/store"
	"agentbox/internal/syncproto"
)

type retirementFixture struct {
	s        *Server
	sess     store.Session
	handler  http.Handler
	mutation syncproto.Mutation
	status   syncproto.OperationStatus
	identity string
}

func newRetirementFixture(t *testing.T, before bool) retirementFixture {
	t.Helper()
	s, sess, handler, lease, mutation := mutationFixture(t)
	if before {
		if err := os.WriteFile(filepath.Join(s.workspaceDir(sess), "file"), []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
		mutation.Before = mutationEntry("old")
	}
	if w := mutationRequest(handler, mutation, lease, strings.NewReader("new\r\n\x00"), "alice"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := s.syncLeases().Release(sess.ID, mutation.Project, ".", mutation.Device, lease.Token, lease.Generation); err != nil {
		t.Fatal(err)
	}
	identity, err := s.clientIdentity()
	if err != nil {
		t.Fatal(err)
	}
	f := retirementFixture{s: s, sess: sess, handler: handler, mutation: mutation, identity: identity}
	w := f.request(http.MethodGet, "operations/"+mutation.ID, nil, "alice", identity)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &f.status) != nil || f.status.Validate(mutation.ID) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return f
}

func (f retirementFixture) request(method, endpoint string, body any, user, identity string) *httptest.ResponseRecorder {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, "/api/sessions/"+f.sess.ID+"/sync/"+endpoint, bytes.NewReader(raw))
	if user != "" {
		r.Header.Set("Authorization", "Bearer client-fixture-"+user)
	}
	if identity != "" {
		r.Header.Set("X-Agentbox-Server-ID", identity)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func (f retirementFixture) confirmation() syncproto.RetireOperationRequest {
	return syncproto.RetireOperationRequest{Device: f.status.Device, Digest: f.status.Digest, Confirmation: f.status.RetirementConfirmation}
}

func (f retirementFixture) storage(t *testing.T) syncproto.RecoveryStorage {
	t.Helper()
	w := f.request(http.MethodGet, "storage", nil, "alice", f.identity)
	var result syncproto.RecoveryStorage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Validate() != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return result
}

func TestSyncRecoveryRetirementPreservesReceiptAndReclaimsCapacity(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprint(before), func(t *testing.T) {
			f := newRetirementFixture(t, before)
			initial := f.storage(t)
			if initial.ActiveOperations != 1 || initial.RetainedReceipts != 0 || before && initial.RecoveryBytes != 3 {
				t.Fatal(initial)
			}
			endpoint := "operations/" + f.mutation.ID + "/retire"
			for attempt := 0; attempt < 2; attempt++ {
				w := f.request(http.MethodPost, endpoint, f.confirmation(), "alice", f.identity)
				var status syncproto.OperationStatus
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Validate(f.mutation.ID) != nil || status.Retirement != "retired" || status.Operation.Status != "applied" || status.Operation.Recovery || status.Digest != f.status.Digest || status.RetirementConfirmation != f.status.RetirementConfirmation {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			final := f.storage(t)
			if final.ActiveOperations != 0 || final.RetainedReceipts != 1 || final.RecoveryBytes != 0 || final.MetadataBytes == 0 {
				t.Fatal(final)
			}
			if w := f.request(http.MethodGet, "operations/"+f.mutation.ID+"/before", nil, "alice", f.identity); w.Code != 404 {
				t.Fatal(w.Code, w.Body.String())
			}
			// A restarted service loses every lease/cache, never its replay fence.
			f.s.clientLeases = syncproto.NewLeases()
			f.s.forgetClientInventory(f.sess.ID)
			if fresh := f.storage(t); fresh != final {
				t.Fatal("cold scan disagrees", fresh, final)
			}
			lease, err := f.s.syncLeases().Acquire(f.sess.ID, f.mutation.Project, ".", f.mutation.Device)
			if err != nil {
				t.Fatal(err)
			}
			f.mutation.Generation = lease.Generation
			file := filepath.Join(f.s.workspaceDir(f.sess), "file")
			if err := os.WriteFile(file, []byte("editor"), 0644); err != nil {
				t.Fatal(err)
			}
			w := mutationRequest(f.handler, f.mutation, lease, strings.NewReader("new\r\n\x00"), "alice")
			var replay syncproto.MutationResult
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &replay) != nil || !replay.Replayed || replay.Recovery {
				t.Fatal(w.Code, w.Body.String())
			}
			if raw, _ := os.ReadFile(file); string(raw) != "editor" {
				t.Fatal("retired replay wrote user file")
			}
			f.mutation.After = mutationEntry("wrong")
			if w := mutationRequest(f.handler, f.mutation, lease, strings.NewReader("wrong"), "alice"); w.Code != 409 {
				t.Fatal("retired ID accepted different intent", w.Code)
			}
		})
	}
}

func TestSyncRecoveryRetirementRejectsUnsafeRequests(t *testing.T) {
	for _, kind := range []string{"missing_identity", "wrong_identity", "wrong_device", "wrong_digest", "wrong_confirmation", "foreign", "unauthenticated", "lease", "uncertain", "corrupt", "missing", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newRetirementFixture(t, true)
			request, user, identity := f.confirmation(), "alice", f.identity
			name := filepath.Join(f.s.sessionDir(f.sess), "client-sync", f.mutation.ID, "before")
			switch kind {
			case "missing_identity":
				identity = ""
			case "wrong_identity":
				identity = strings.Repeat("f", 64)
			case "wrong_device":
				request.Device = "other-device"
			case "wrong_digest":
				request.Digest = strings.Repeat("f", 64)
			case "wrong_confirmation":
				request.Confirmation = strings.Repeat("f", 64)
			case "foreign":
				user = "bob"
			case "unauthenticated":
				user = ""
			case "lease":
				// Even another project's lease prevents removing an executor's bytes.
				_, err := f.s.syncLeases().Acquire(f.sess.ID, "other-project", "other", "other-device")
				if err != nil {
					t.Fatal(err)
				}
			case "uncertain":
				root, err := safefs.Open(filepath.Dir(name))
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				record, err := loadClientMutationRecord(root, f.mutation.ID)
				if err != nil {
					t.Fatal(err)
				}
				record.Result.Status = "uncertain"
				if err := saveClientMutation(root, record); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(name, []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(f.s.workspaceDir(f.sess), "file"), name); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(name, name+"-link"); err != nil {
					t.Fatal(err)
				}
			}
			f.s.forgetClientInventory(f.sess.ID)
			w := f.request(http.MethodPost, "operations/"+f.mutation.ID+"/retire", request, user, identity)
			if w.Code == 200 {
				t.Fatal("unsafe retirement accepted", kind)
			}
			if raw, _ := os.ReadFile(filepath.Join(f.s.workspaceDir(f.sess), "file")); string(raw) != "new\r\n\x00" {
				t.Fatal("cleanup changed current user file")
			}
			if kind != "missing" {
				if _, err := os.Lstat(name); err != nil {
					t.Fatal("cleanup removed recovery", err)
				}
			}
		})
	}
}

func TestSyncRecoveryRetirementResumesEveryDurableBoundary(t *testing.T) {
	for _, stage := range []string{"before_intent", "after_intent", "before_unlink", "after_unlink", "after_unlink_sync", "after_retired"} {
		t.Run(stage, func(t *testing.T) {
			f := newRetirementFixture(t, true)
			root, err := safefs.Open(filepath.Join(f.s.sessionDir(f.sess), "client-sync", f.mutation.ID))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			record, err := loadClientMutationRecord(root, f.mutation.ID)
			if err != nil {
				t.Fatal(err)
			}
			crashed := errors.New("injected process loss")
			err = retireClientOperation(t.Context(), root, &record, func(current string) error {
				if current == stage {
					return crashed
				}
				return nil
			})
			if !errors.Is(err, crashed) {
				t.Fatal(err)
			}
			f.s.forgetClientInventory(f.sess.ID)
			w := f.request(http.MethodPost, "operations/"+f.mutation.ID+"/retire", f.confirmation(), "alice", f.identity)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			storage := f.storage(t)
			if storage.ActiveOperations != 0 || storage.RetainedReceipts != 1 || storage.RecoveryBytes != 0 {
				t.Fatal(storage)
			}
		})
	}
}

func TestSyncRecoveryRetirementWorksAfterProjectRemovalAndInvalidatesRootCache(t *testing.T) {
	f := newRetirementFixture(t, true)
	if err := f.s.store.DeleteClientProject(f.sess.ID, f.mutation.Project, f.mutation.Revision); err != nil {
		t.Fatal(err)
	}
	w := f.request(http.MethodPost, "operations/"+f.mutation.ID+"/retire", f.confirmation(), "alice", f.identity)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	old := f.storage(t)
	journal := filepath.Join(f.s.sessionDir(f.sess), "client-sync")
	if err := os.Rename(journal, journal+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journal, 0700); err != nil {
		t.Fatal(err)
	}
	fresh := f.storage(t)
	if old.RetainedReceipts != 1 || fresh.RetainedReceipts != 0 || fresh.ActiveOperations != 0 {
		t.Fatal(old, fresh)
	}
}

func TestSyncRecoveryRetirementOldApplyRecordShapeStillFencesReplay(t *testing.T) {
	f := newRetirementFixture(t, true)
	w := f.request(http.MethodPost, "operations/"+f.mutation.ID+"/retire", f.confirmation(), "alice", f.identity)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Freeze the old reader's fields and acceptance logic: unknown retirement is
	// ignored, but existing ID + matching digest + applied means return receipt.
	var old struct {
		Digest  string                   `json:"digest"`
		Request syncproto.Mutation       `json:"request"`
		Result  syncproto.MutationResult `json:"result"`
	}
	raw, err := os.ReadFile(filepath.Join(f.s.sessionDir(f.sess), "client-sync", f.mutation.ID, "record.json"))
	if err != nil || json.Unmarshal(raw, &old) != nil {
		t.Fatal(err)
	}
	digest, err := f.mutation.Digest()
	if err != nil || old.Result.ID != f.mutation.ID || old.Digest != digest || old.Result.Status != "applied" || old.Result.Recovery {
		t.Fatal("old apply would not return its existing receipt", old, err)
	}
}

func TestSyncRecoveryInventoryQuotaAndRetiringReservation(t *testing.T) {
	f := newRetirementFixture(t, false)
	journal, err := safefs.Open(filepath.Join(f.s.sessionDir(f.sess), "client-sync"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	root, err := safefs.Open(f.s.workspaceDir(f.sess))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	inventory, err := scanClientInventory(t.Context(), journal)
	if err != nil {
		t.Fatal(err)
	}
	inventory.storage.ActiveOperations = clientJournalLimit
	request := f.mutation
	request.ID = strings.Repeat("b", 32)
	request.Path = "new"
	if _, err := applyClientMutation(t.Context(), root, journal, request, strings.NewReader("new\r\n\x00"), func() error { return nil }, inventory); !errors.Is(err, syncproto.ErrLimit) {
		t.Fatal(err)
	}
	// Exercise the HTTP limit without creating 100000 pointless test directories.
	cached, err := f.s.clientInventory(t.Context(), f.sess.ID, journal)
	if err != nil {
		t.Fatal(err)
	}
	cached.storage.RetainedReceipts = clientReceiptLimit
	w := f.request(http.MethodPost, "operations/"+f.mutation.ID+"/retire", f.confirmation(), "alice", f.identity)
	if w.Code != 413 {
		t.Fatal(w.Code, w.Body.String())
	}
	operation, err := journal.Sub(f.mutation.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer operation.Close()
	record, err := loadClientMutationRecord(operation, f.mutation.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = retireClientOperation(t.Context(), operation, &record, func(stage string) error {
		if stage == "after_intent" {
			return context.Canceled
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.s.forgetClientInventory(f.sess.ID)
	pending := f.storage(t)
	if pending.ActiveOperations != 1 || pending.RetainedReceipts != 1 {
		t.Fatal(pending)
	}
	cached, err = f.s.clientInventory(t.Context(), f.sess.ID, journal)
	if err != nil {
		t.Fatal(err)
	}
	cached.storage.RetainedReceipts = clientReceiptLimit
	w = f.request(http.MethodPost, "operations/"+f.mutation.ID+"/retire", f.confirmation(), "alice", f.identity)
	if w.Code != 200 {
		t.Fatal("reserved retirement must finish at quota", w.Code, w.Body.String())
	}
	if _, err := applyClientMutation(t.Context(), root, journal, request, strings.NewReader("new\r\n\x00"), func() error { return nil }, cached); err != nil {
		t.Fatal("retired slot not available", err)
	}
}
