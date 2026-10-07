package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/safefs"
)

type fakeRuntime struct {
	pingErr, imageErr error
	calls             []string
	cancel            func()
}

func (f *fakeRuntime) DiagnosticPing(ctx context.Context) error {
	f.calls = append(f.calls, "ping")
	if f.cancel != nil {
		f.cancel()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return f.pingErr
}
func (f *fakeRuntime) DiagnosticImage(ctx context.Context, _ string) error {
	f.calls = append(f.calls, "image")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return f.imageErr
}

func fixture(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{"auth_token":"synthetic-secret","data_dir":".","timezone":"UTC","accounts":[{"id":"private-account","type":"claude","env":{"ANTHROPIC_API_KEY":"private-key"}}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
func find(t *testing.T, r Report, id string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatal("missing check", id)
	return Check{}
}

func TestDiagnosticStatesAndNoSecretOrModelClaims(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rt            *fakeRuntime
		docker, image State
		imageCode     string
	}{
		{"ready", &fakeRuntime{}, Passed, Passed, "image_ok"},
		{"daemon", &fakeRuntime{pingErr: errors.New("private-key /private/docker.sock")}, Failed, NotChecked, "dependency_unavailable"},
		{"image missing", &fakeRuntime{imageErr: fs.ErrNotExist}, Passed, Failed, "image_missing"},
		{"image forbidden", &fakeRuntime{imageErr: errors.New("private-account private-key")}, Passed, Failed, "image_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixture(t)
			r := Instance(t.Context(), cfg, tc.rt, false)
			if find(t, r, "docker").State != tc.docker || find(t, r, "agent_image").State != tc.image || find(t, r, "agent_image").Code != tc.imageCode {
				t.Fatal(r)
			}
			if find(t, r, "model").State != NotChecked || find(t, r, "websocket").State != NotChecked || find(t, r, "data_permissions").State != NotChecked {
				t.Fatal("unperformed probe claimed passed")
			}
			if find(t, r, "account_configuration").State != Passed {
				t.Fatal("relay credentials not recognized")
			}
			if tc.docker == Failed && !reflect.DeepEqual(tc.rt.calls, []string{"ping"}) {
				t.Fatal("image probe ran after failed ping")
			}
			raw, _ := json.Marshal(r)
			for _, secret := range []string{"private-key", "private-account", "synthetic-secret", cfg.DataDir, "/private/docker.sock"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("private diagnostic data leaked")
				}
			}
		})
	}
	cfg := fixture(t)
	cfg.Resources.MinFreeBytes = 1 << 62
	r := Instance(t.Context(), cfg, nil, false)
	if find(t, r, "data_disk").Code != "storage_full" || find(t, r, "docker").State != NotChecked {
		t.Fatal(r)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rt := &fakeRuntime{}
	if checks := RuntimeChecks(ctx, rt, "private-image"); checks[0].State != NotChecked || len(rt.calls) != 0 {
		t.Fatal("cancelled probe ran")
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	rt = &fakeRuntime{cancel: cancel}
	if checks := RuntimeChecks(ctx, rt, "private-image"); checks[0].State != NotChecked || checks[0].Code != "check_cancelled" || len(rt.calls) != 1 {
		t.Fatal("interrupted probe reported daemon failure or continued")
	}
}

func TestDiagnosticWriteProbePreservesFilesAndCleansUp(t *testing.T) {
	cfg := fixture(t)
	root, err := safefs.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	original, err := os.ReadFile(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	checks := ProbeWrite(t.Context(), root)
	if checks[0].State != Passed {
		t.Fatal(checks)
	}
	if runtime.GOOS != "linux" && checks[1].State != NotChecked {
		t.Fatal("non-Linux ownership claimed passed")
	}
	if runtime.GOOS == "linux" && (os.Geteuid() == 0 || os.Geteuid() == 1000) && checks[1].State != Passed {
		t.Fatal("ownership capability not verified")
	}
	if runtime.GOOS == "linux" && os.Geteuid() != 0 && os.Geteuid() != 1000 && checks[1].State != Failed {
		t.Fatal("ownership denial concealed")
	}
	after, err := os.ReadDir(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("probe left temporary files")
	}
	for i := range before {
		if before[i].Name() != after[i].Name() {
			t.Fatal("directory contents changed")
		}
	}
	now, _ := os.ReadFile(cfg.Path())
	if string(now) != string(original) {
		t.Fatal("probe changed config")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if checks := ProbeWrite(ctx, root); checks[0].State != NotChecked {
		t.Fatal("cancelled write probe ran")
	}
}

func TestDiagnosticWorkspaceRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "workspace")); err != nil {
		t.Fatal(err)
	}
	root, err := safefs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if check := WorkspacePermissions(root, "workspace"); check.State != Failed {
		t.Fatal("followed container-controlled link")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("modified link target")
	}
}

func TestDiagnosticVocabularyContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/messages-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string][2]string
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expected, Messages) {
		t.Fatal("diagnostic vocabulary changed without contract update")
	}
	if result := Result("model", Passed, "unknown-private-error"); result.State != NotChecked || result.Code != "not_requested" {
		t.Fatal("unknown code leaked")
	}
}
