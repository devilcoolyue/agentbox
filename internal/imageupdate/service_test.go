package imageupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/config"
)

type fakeBackend struct {
	images      map[string]Image
	builds      int
	buildErr    error
	beforeBuild func()
	corrupt     bool
}

func (b *fakeBackend) InspectCLIImage(_ context.Context, ref string) (Image, error) {
	v, ok := b.images[ref]
	if !ok {
		return Image{}, errors.New("image missing")
	}
	return v, nil
}
func (b *fakeBackend) BuildCLIImage(ctx context.Context, base Image, v Versions, tag string, log func(string)) error {
	b.builds++
	if b.beforeBuild != nil {
		b.beforeBuild()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.buildErr != nil {
		return b.buildErr
	}
	log(strings.Repeat("test output\n", 3000))
	base.ID = "new-id"
	base.Claude = v.Claude
	base.Codex = v.Codex
	if b.corrupt {
		base.Browser = false
	}
	b.images[tag] = base
	b.images[base.ID] = base
	return nil
}
func fixture(t *testing.T) (*Service, *fakeBackend, *int) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:8180","auth_token":"test-test-test-test","data_dir":".","agent_image":"browser","max_upload_mb":10,"image_updates":{"enabled":true,"channel":"latest","time":"00:00"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	original := Image{ID: "old-id", Claude: "2.1.280", Codex: "0.156.1", Browser: true}
	b := &fakeBackend{images: map[string]Image{"browser": original, "old-id": original}}
	s := New(cfg, b)
	requests := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests++
		switch r.URL.Path {
		case "/@anthropic-ai/claude-code/latest":
			fmt.Fprint(w, `{"version":"2.1.285"}`)
		case "/@anthropic-ai/claude-code/stable":
			fmt.Fprint(w, `{"version":"2.1.280"}`)
		case "/@openai/codex/latest":
			fmt.Fprint(w, `{"version":"0.157.0"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	s.registry = server.URL + "/"
	return s, b, requests
}
func run(t *testing.T, s *Service, action string) {
	t.Helper()
	job, err := s.Prepare(action, false)
	if err != nil {
		t.Fatal(err)
	}
	job(context.Background())
}
func TestCheckUpdateRollback(t *testing.T) {
	s, b, requests := fixture(t)
	run(t, s, "check")
	st := s.Snapshot()
	if !st.Available || st.Error != "" || b.builds != 0 || *requests != 1 {
		t.Fatalf("check: %+v", st)
	}
	run(t, s, "update")
	st = s.Snapshot()
	if st.Error != "" || st.ResultImage == "" || b.builds != 1 || len(st.Log) > 16100 {
		t.Fatalf("update: %+v", st)
	}
	_, active, previous := s.cfg.ImageUpdateState()
	if active != st.ResultImage || previous != "old-id" {
		t.Fatalf("activation: %s %s", active, previous)
	}
	fresh := New(s.cfg, b)
	if fresh.Snapshot().ResultImage != active {
		t.Fatal("status not persisted")
	}
	run(t, s, "update")
	if b.builds != 1 {
		t.Fatal("rebuilt unchanged image")
	}
	run(t, s, "rollback")
	policy, active, _ := s.cfg.ImageUpdateState()
	if policy.Enabled || active != "old-id" {
		t.Fatalf("rollback: %+v %s", policy, active)
	}
}
func TestFailuresNeverSwitch(t *testing.T) {
	for _, kind := range []string{"build", "validation", "policy", "image", "cancel", "registry"} {
		t.Run(kind, func(t *testing.T) {
			s, b, _ := fixture(t)
			switch kind {
			case "build":
				b.buildErr = errors.New("npm failed")
			case "validation":
				b.corrupt = true
			case "policy":
				b.beforeBuild = func() {
					p, _, _ := s.cfg.ImageUpdateState()
					p.Enabled = false
					if err := s.cfg.ApplySettings(config.SettingsPatch{ImageUpdates: &p}); err != nil {
						t.Fatal(err)
					}
				}
			case "image":
				b.beforeBuild = func() {
					next := "manual"
					if err := s.cfg.ApplySettings(config.SettingsPatch{AgentImage: &next}); err != nil {
						t.Fatal(err)
					}
				}
			case "registry":
				s.registry += "missing/"
			}
			job, err := s.Prepare("update", false)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancel" {
				cancel()
			}
			job(ctx)
			if s.Snapshot().Error == "" {
				t.Fatal("missing failure")
			}
			_, active, previous := s.cfg.ImageUpdateState()
			want := "browser"
			if kind == "image" {
				want = "manual"
			}
			if active != want || previous != "" {
				t.Fatalf("changed active image: %s %s", active, previous)
			}
		})
	}
}
func TestPolicyAndScheduling(t *testing.T) {
	s, b, requests := fixture(t)
	p, _, _ := s.cfg.ImageUpdateState()
	p.Channel = "stable"
	if err := s.cfg.ApplySettings(config.SettingsPatch{ImageUpdates: &p}); err != nil {
		t.Fatal(err)
	}
	run(t, s, "update")
	if b.builds != 0 || s.Snapshot().Available {
		t.Fatal("stable unexpectedly updated")
	}
	p.UpdateCodex = true
	if err := s.cfg.ApplySettings(config.SettingsPatch{ImageUpdates: &p}); err != nil {
		t.Fatal(err)
	}
	run(t, s, "update")
	if b.builds != 1 || s.Snapshot().Target.Claude != "2.1.280" || s.Snapshot().Target.Codex != "0.157.0" {
		t.Fatal("Codex selection failed")
	}
	job, err := s.Prepare("update", true)
	if err != nil || job == nil {
		t.Fatalf("scheduled: %v", err)
	}
	if _, err := s.Prepare("check", false); err == nil {
		t.Fatal("concurrent job admitted")
	}
	// A crash before completion retains the daily reservation and exposes an error.
	restarted := New(s.cfg, b)
	if restarted.Snapshot().Running || restarted.Snapshot().Error == "" {
		t.Fatal("restart did not recover")
	}
	if next, err := restarted.Prepare("update", true); err != nil || next != nil {
		t.Fatal("same day re-run after restart")
	}
	job(context.Background())
	p, _, _ = s.cfg.ImageUpdateState()
	p.Enabled = false
	if err := s.cfg.ApplySettings(config.SettingsPatch{ImageUpdates: &p}); err != nil {
		t.Fatal(err)
	}
	before := *requests
	if next, err := s.Prepare("update", true); err != nil || next != nil {
		t.Fatal("disabled job admitted")
	}
	if *requests != before {
		t.Fatal("disabled updater accessed registry")
	}
}
func TestScheduleTimeZoneAndVersions(t *testing.T) {
	utc := time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC)
	zone := time.FixedZone("UTC+8", 8*3600)
	if !Due(utc.In(zone), "04:00", "2026-09-30") || Due(utc.In(zone), "04:01", "") || Due(utc.In(zone), "04:00", "2026-10-01") {
		t.Fatal("wall clock schedule incorrect")
	}
	for _, pair := range [][2]string{{"2.1.280", "2.1.285"}, {"2.1.285", "2.1.285"}, {"2.1.99999999999999999999999", "2.1.280"}, {"2.1.285;touch /tmp/x", "2.1.280"}} {
		if newer(pair[0], pair[1]) {
			t.Fatalf("downgrade/invalid accepted: %v", pair)
		}
	}
	if !newer("2.1.288", "2.1.99") {
		t.Fatal("version comparison was lexical")
	}
}
