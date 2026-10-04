package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/store"
	"agentbox/internal/syncclient"
)

// Opt-in: launches only an explicit desktop-smoke bundle against synthetic data.
// No Docker, real credentials, model calls, production server or user mappings.
func TestDesktopSyncNativeSmoke(t *testing.T) {
	binary := os.Getenv("AGENTBOX_SYNC_SMOKE_BINARY")
	if binary == "" {
		t.Skip("set AGENTBOX_SYNC_SMOKE_BINARY to an explicit desktop-smoke executable")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateDir, exportDir := filepath.Join(private, "state"), filepath.Join(private, "export")
	for _, path := range []string{stateDir, exportDir} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var lost, block atomic.Bool
	var applied, blocked, canceled atomic.Int32
	var historySeeded atomic.Int32
	var historySeedMu sync.Mutex
	var verified atomic.Bool
	var f *engineFixture
	f = newEngineFixture(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/sessions" && r.Method == "GET" && r.Header.Get("Authorization") == "Bearer client-fixture-alice" {
				other := f.session
				other.ID = "smoke-other"
				other.Name = "Other workspace"
				json.NewEncoder(w).Encode([]store.Session{f.session, other})
				return
			}
			if r.URL.Path == "/api/login" && r.Method == "POST" {
				var login struct{ Username, Password string }
				if json.NewDecoder(r.Body).Decode(&login) != nil || login.Username != "alice" || login.Password != "synthetic-password" {
					w.WriteHeader(401)
					return
				}
				fmt.Fprint(w, `{"token":"client-fixture-alice"}`)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/__smoke/") {
				if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer client-fixture-alice" {
					w.WriteHeader(401)
					return
				}
				switch strings.TrimPrefix(r.URL.Path, "/__smoke/") {
				case "orphan_seed":
					status := seedOrphanRecoveryFixture(t, f)
					json.NewEncoder(w).Encode(status)
					return
				case "verify_remote_cleanup":
					root, err := f.server.openDataDir(f.server.sessionDir(f.session))
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					defer root.Close()
					journal, err := root.Sub("client-sync")
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					defer journal.Close()
					entries, err := journal.ReadDir(".")
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					retired := 0
					for _, entry := range entries {
						dir, err := journal.Sub(entry.Name())
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						record, err := loadClientMutationRecord(dir, entry.Name())
						dir.Close()
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						if record.Retirement == "retired" {
							retired++
							if _, err = journal.Lstat(entry.Name() + "/before"); !os.IsNotExist(err) {
								t.Error("retired copy still present", err)
								w.WriteHeader(500)
								return
							}
						}
					}
					if retired == 0 {
						t.Error("UI did not retire remote recovery")
						w.WriteHeader(500)
						return
					}
				case "stale":
					writeEngineFile(t, f.local, "local.txt", "stale-preview-new-bytes")
				case "lost":
					writeEngineFile(t, f.local, "local.txt", "second-version")
					lost.Store(true)
				case "review_stale":
					writeEngineFile(t, f.local, "local.txt", "changed-after-review")
				case "review_restore":
					writeEngineFile(t, f.local, "local.txt", "second-version")
				case "download":
					writeEngineFile(t, f.remote, "remote.txt", "remote-new\n")
				case "partial":
					writeEngineFile(t, f.local, "a.txt", "partial-first")
					writeEngineFile(t, f.local, "b.txt", "partial-second")
					lost.Store(true)
				case "history_seed":
					// One fully verified batch per bounded fixture request. The
					// native helper has a 10-second deadline; committing all 21
					// snapshots in one request made setup race that deadline.
					historySeedMu.Lock()
					defer historySeedMu.Unlock()
					if historySeeded.Load() >= 21 {
						t.Error("history fixture seeded more than 21 batches")
						w.WriteHeader(http.StatusConflict)
						return
					}
					state, err := syncclient.OpenStateContext(r.Context(), stateDir)
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					defer state.Close()
					bindings, err := state.Bindings(f.saved.Binding.ServerID, "alice")
					if err != nil || len(bindings) != 1 {
						t.Error("history seed binding", err)
						w.WriteHeader(500)
						return
					}
					engine := &syncclient.Engine{State: state, Remote: f.engine.Remote}
					preview, err := engine.Preview(r.Context(), bindings[0].ID, syncclient.Automatic)
					if err != nil || len(preview.Plan.Operations) != 0 {
						t.Error("history seed preview", err)
						w.WriteHeader(500)
						return
					}
					if _, err = engine.Apply(r.Context(), bindings[0].ID, preview, preview.Plan.Digest); err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					seeded := historySeeded.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]int32{"seeded": seeded, "total": 21})
					return
				case "choices":
					for _, name := range []string{"choice-local.txt", "choice-remote.txt"} {
						writeEngineFile(t, f.local, name, "local-choice")
						writeEngineFile(t, f.remote, name, "remote-choice")
					}
				case "abandon_lost":
					writeEngineFile(t, f.local, "a.txt", "unknown-final")
					lost.Store(true)
				case "continuous":
					writeEngineFile(t, f.local, "continuous.txt", "automatic")
				case "continuous_done":
					raw, _ := os.ReadFile(filepath.Join(f.remote, "continuous.txt"))
					_ = json.NewEncoder(w).Encode(map[string]bool{"done": string(raw) == "automatic"})
					return
				case "continuous_conflict":
					writeEngineFile(t, f.local, "continuous.txt", "local-conflict")
					writeEngineFile(t, f.remote, "continuous.txt", "remote-conflict")
				case "continuous_restore":
					writeEngineFile(t, f.local, "continuous.txt", "automatic")
					writeEngineFile(t, f.remote, "continuous.txt", "automatic")
				case "block":
					block.Store(true)
				case "blocked":
					_ = json.NewEncoder(w).Encode(map[string]int32{"blocked": blocked.Load()})
					return
				case "verify":
					if canceled.Load() != 2 || applied.Load() != 7 || historySeeded.Load() != 21 {
						t.Errorf("requests: canceled=%d applied=%d history_seeded=%d", canceled.Load(), applied.Load(), historySeeded.Load())
						w.WriteHeader(500)
						return
					}
					if err := verifyDesktopSyncFixture(f, stateDir, exportDir); err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					verified.Store(true)
				default:
					w.WriteHeader(400)
					return
				}
				fmt.Fprint(w, `{}`)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/sync/manifest") && block.Swap(false) {
				blocked.Add(1)
				select {
				case <-r.Context().Done():
					canceled.Add(1)
				case <-time.After(15 * time.Second):
					t.Error("native cancellation did not close HTTP request")
				}
				return
			}
			if strings.HasSuffix(r.URL.Path, "/sync/file") {
				response := httptest.NewRecorder()
				next.ServeHTTP(response, r)
				for key, values := range response.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(response.Code)
				raw := response.Body.Bytes()
				split := len(raw) / 2
				_, _ = w.Write(raw[:split])
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(400 * time.Millisecond):
				}
				_, _ = w.Write(raw[split:])
				return
			}
			if strings.HasSuffix(r.URL.Path, "/sync/apply") {
				applied.Add(1)
				if lost.Swap(false) {
					response := httptest.NewRecorder()
					next.ServeHTTP(response, r)
					if response.Code != 200 {
						t.Error("fixture apply", response.Code, response.Body.String())
					}
					w.Header().Set("X-Agentbox-Server-ID", response.Header().Get("X-Agentbox-Server-ID"))
					w.WriteHeader(503)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	other := f.session
	other.ID = "smoke-other"
	other.Name = "Other workspace"
	if err := f.server.store.Put(other); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.server.workspaceDir(other), 0700); err != nil {
		t.Fatal(err)
	}
	writeEngineFile(t, f.local, "local.txt", "initial-local")
	writeEngineFile(t, f.remote, "remote.txt", "remote\r\n中文\x00")
	config, _ := json.Marshal(map[string]string{"server": f.saved.Binding.Server, "local": f.local, "state": stateDir, "export": exportDir})
	report := filepath.Join(private, "report.json")
	// The expanded recovery/continuous-sync workflow includes 21 real durable
	// history commits. Python stops at 170s, leaving time for owned-app cleanup;
	// per-request and per-UI-step limits remain unchanged.
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary)
	if runtime.GOOS == "darwin" {
		runner, err := filepath.Abs(filepath.Join("..", "..", "desktop", "scripts", "smoke.py"))
		if err != nil {
			t.Fatal(err)
		}
		command = exec.CommandContext(ctx, "python3", runner, binary, "--sync-fixture")
	}
	command.Env = append(os.Environ(), "AGENTBOX_SMOKE_REPORT="+report, "AGENTBOX_SMOKE_SYNC="+string(config))
	output, runErr := command.CombinedOutput()
	raw, readErr := os.ReadFile(report)
	if runErr != nil || readErr != nil {
		stages, _ := os.ReadFile(filepath.Join(private, "report.stages"))
		window, _ := os.ReadFile(filepath.Join(private, "report.window.jsonl"))
		t.Fatalf("native smoke: %v report: %v\nstages: %s\nwindow: %s\n%s\n%s", runErr, readErr, stages, window, raw, output)
	}
	var result struct {
		OK   bool `json:"ok"`
		Sync bool `json:"sync_mode"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.OK || !result.Sync || !verified.Load() {
		t.Fatalf("native smoke not verified: %s", raw)
	}
	t.Log(string(raw))
}

func verifyDesktopSyncFixture(f *engineFixture, stateDir, exportDir string) error {
	for _, root := range []string{f.local, f.remote} {
		for path, want := range map[string]string{"local.txt": "second-version", "remote.txt": "remote-new\n", "a.txt": "unknown-final", "b.txt": "partial-second", "choice-local.txt": "local-choice", "choice-remote.txt": "remote-choice", "continuous.txt": "automatic"} {
			raw, err := os.ReadFile(filepath.Join(root, path))
			if err != nil || string(raw) != want {
				return fmt.Errorf("final file %s: %v", path, err)
			}
		}
		exports, err := filepath.Glob(filepath.Join(root, "agentbox-recovery-*.bak"))
		if err != nil || len(exports) != 0 {
			return fmt.Errorf("export entered mapped tree: %v", err)
		}
	}
	files, err := os.ReadDir(exportDir)
	if err != nil || len(files) != 3 {
		return fmt.Errorf("export count: %v", err)
	}
	originals := map[string]bool{"stale-preview-new-bytes": true, "remote\r\n中文\x00": true, "orphan-recovery-before\r\n中文\x00": true}
	for _, file := range files {
		raw, err := os.ReadFile(filepath.Join(exportDir, file.Name()))
		if err != nil || !originals[string(raw)] {
			return fmt.Errorf("recovery bytes changed: %v", err)
		}
		delete(originals, string(raw))
	}
	// The independent recovery UI must leave the current file untouched and
	// retain the original applied receipt while removing only its before bytes.
	orphanID := strings.Repeat("f", 32)
	root, err := f.server.openDataDir(filepath.Join(f.server.sessionDir(f.session), "client-sync", orphanID))
	if err != nil {
		return err
	}
	defer root.Close()
	record, err := loadClientMutationRecord(root, orphanID)
	if err != nil || record.Retirement != "retired" || record.Result.Status != "applied" {
		return fmt.Errorf("orphan recovery receipt not retained: %v", err)
	}
	if _, err := root.Lstat("before"); !os.IsNotExist(err) {
		return fmt.Errorf("orphan recovery content not removed: %v", err)
	}
	current, err := os.ReadFile(filepath.Join(f.remote, "orphan-recovery-fixture", "old.txt"))
	if err != nil || string(current) != "orphan latest\r\n" {
		return fmt.Errorf("orphan recovery modified current file: %v", err)
	}
	state, err := syncclient.OpenState(stateDir)
	if err != nil {
		return err
	}
	defer state.Close()
	bindings, err := state.Bindings(f.saved.Binding.ServerID, "alice")
	if err != nil || len(bindings) != 3 {
		return fmt.Errorf("native binding count: %v", err)
	}
	if err := f.server.store.CreateToken("client-fixture-audit", "alice"); err != nil {
		return err
	}
	remote, err := syncclient.NewRemote(f.saved.Binding.Server, "client-fixture-audit", false)
	if err != nil {
		return err
	}
	defer remote.Close()
	engine := syncclient.Engine{State: state, Remote: remote}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var saved syncclient.SavedBinding
	active := 0
	for _, binding := range bindings {
		if binding.Archived {
			history, historyErr := engine.RecoveryHistoryPage(ctx, binding.ID, "")
			if historyErr != nil {
				return historyErr
			}
			if history.History[0].Action == "abandoned" {
				if history.Metadata.HistoryBatches != 4 {
					return fmt.Errorf("continuous sync created unexpected history: %d", history.Metadata.HistoryBatches)
				}
				batch, err := state.History(binding.ID, history.History[0].ID)
				if err != nil || batch.Operations[0].Status != "started" || batch.Resolution.LocalDigest != "" {
					return fmt.Errorf("unknown batch lost: %v", err)
				}
			} else {
				saved = binding
				if history.Metadata.HistoryBatches != 25 {
					return fmt.Errorf("safe cleanup count: %d", history.Metadata.HistoryBatches)
				}
			}
		} else {
			active++
			history, historyErr := engine.RecoveryHistoryPage(ctx, binding.ID, "")
			if historyErr != nil || history.Metadata.HistoryBatches != 1 {
				return fmt.Errorf("continuous sync created unexpected history: %d, %v", history.Metadata.HistoryBatches, historyErr)
			}
			if binding.Baseline == nil || binding.Pending != nil {
				return fmt.Errorf("rebound baseline invalid")
			}
		}
	}
	if active != 1 || saved.ID == "" {
		return fmt.Errorf("archive and new binding missing")
	}
	if saved.Pending != nil || saved.Baseline == nil {
		return fmt.Errorf("native baseline not committed")
	}
	history, err := engine.RecoveryHistory(ctx, saved.ID)
	if err != nil {
		return err
	}
	actions := map[string]int{}
	for _, batch := range history {
		actions[batch.Action]++
	}
	if actions["finish"] != 1 || actions["replan"] != 1 {
		return fmt.Errorf("missing durable resolutions: %v", actions)
	}
	return nil
}
