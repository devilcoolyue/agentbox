package backup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"agentbox/internal/store"
)

func TestBackupRestorePreservesChatReceipts(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			cfg, data := fixture(t)
			st, err := store.Open(filepath.Join(data, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			u, _ := st.GetUser("alice")
			originals := map[string]store.ChatRequest{}
			for _, state := range []string{store.ChatAccepted, store.ChatRunning, store.ChatCompleted, store.ChatAbandoned, store.ChatDeleted} {
				if err = st.Put(store.Session{ID: state, User: u.Name, Agent: "codex"}); err != nil {
					t.Fatal(err)
				}
				if state == store.ChatAbandoned {
					if err = st.AbandonUnknownChatRequest(u, state, "request"); err != nil {
						t.Fatal(err)
					}
				} else {
					c, _, err := st.AcceptChatRequest(u, state, "request", store.ChatRequestInput{Scope: "original-instance-scope", ThreadID: "thread", Text: "synthetic accepted prompt", Model: "frozen-model", Effort: "high", EffortControl: "native", Attachments: []string{"/shared/.file/fixture.txt"}})
					if err != nil {
						t.Fatal(err)
					}
					if state == store.ChatRunning || state == store.ChatCompleted {
						for _, next := range []string{store.ChatStarting, store.ChatRunning} {
							c, err = st.AdvanceChatRequest(u, state, c.RequestID, c.Revision, next, "")
							if err != nil {
								t.Fatal(err)
							}
						}
					}
					if state == store.ChatCompleted {
						if _, err = st.AdvanceChatRequest(u, state, c.RequestID, c.Revision, state, ""); err != nil {
							t.Fatal(err)
						}
					}
					if state == store.ChatDeleted {
						if err = st.Delete(state); err != nil {
							t.Fatal(err)
						}
					}
				}
				originals[state], err = st.ChatRequest(u, state, "request")
				if err != nil {
					t.Fatal(err)
				}
			}
			archive := filepath.Join(t.TempDir(), "backup.tar.gz")
			if _, err = Create(t.Context(), Options{Config: cfg, Output: archive, Full: full, CheckStopped: func(context.Context, []string) error { return nil }}); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "restored")
			if _, err = Restore(t.Context(), archive, target); err != nil {
				t.Fatal(err)
			}
			restored, err := store.Open(filepath.Join(target, "data/state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			for state, original := range originals {
				c, err := restored.ChatRequest(u, state, "request")
				if err != nil || !reflect.DeepEqual(c, original) {
					t.Fatal("snapshot lost receipt", c, err)
				}
			}
			if err = restored.RecoverChatRequests(); err != nil {
				t.Fatal(err)
			}
			for state, original := range originals {
				c, err := restored.ChatRequest(u, state, "request")
				if err != nil {
					t.Fatal(err)
				}
				want := state
				if state == store.ChatAccepted || state == store.ChatRunning {
					want = store.ChatUncertain
				}
				if c.State != want || c.TurnID != original.TurnID || c.Fingerprint != original.Fingerprint || !reflect.DeepEqual(c.Request, original.Request) {
					t.Fatal("recovery changed envelope/ID or guessed outcome", c)
				}
				input := store.ChatRequestInput{Scope: "original-instance-scope", ThreadID: "thread", Text: "late request"}
				if c.Request != nil {
					input = *c.Request
				}
				replay, fresh, err := restored.AcceptChatRequest(u, state, "request", input)
				if state == store.ChatDeleted || state == store.ChatAbandoned {
					if fresh || !errors.Is(err, store.ErrChatGone) || replay.Request != nil {
						t.Fatal("tombstone allowed replay", replay, fresh, err)
					}
				} else if err != nil || fresh || !reflect.DeepEqual(c, replay) {
					t.Fatal("restored receipt allowed repeat execution", replay, fresh, err)
				}
				if want == store.ChatUncertain {
					if _, fresh, err = restored.AcceptChatRequest(u, state, "new-id", input); fresh || !errors.Is(err, store.ErrChatPending) {
						t.Fatal("uncertain receipt failed to block", fresh, err)
					}
				}
			}
		})
	}
}
