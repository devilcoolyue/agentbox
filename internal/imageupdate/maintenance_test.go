package imageupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentbox/internal/config"
)

func TestLegacyMigrationPrecedesUpdateAndSurvivesRestart(t *testing.T) {
	s, b, requests := fixture(t)
	s.status.Images = nil
	run(t, s, "update")
	_, active, previous := s.cfg.ImageUpdateState()
	if active != "flat-id" || previous != "old-id" || *requests != 0 || b.builds != 0 || b.probes != 1 {
		t.Fatalf("migration changed versions or skipped validation: %+v", s.Snapshot())
	}
	fresh := New(s.cfg, b)
	fresh.registry = s.registry
	run(t, fresh, "update")
	if b.buildBase != "flat-id" || b.builds != 1 {
		t.Fatal("did not use fixed base after restart")
	}
	// Pretend a later release is available, then verify the second update still
	// starts from flat-id and keeps the current (newer-than-base) Codex version.
	current := b.images["new-id"]
	current.Claude = "2.1.281"
	current.Codex = "0.157.0"
	b.images["new-id"] = current
	run(t, fresh, "update")
	if b.buildBase != "flat-id" || b.buildTarget.Codex != "0.157.0" {
		t.Fatal("chained update or Codex downgrade")
	}
	run(t, fresh, "rollback")
	paused := fresh.Snapshot().MaintenancePaused
	fresh = New(s.cfg, b)
	fresh.registry = s.registry
	run(t, fresh, "maintain")
	_, active, _ = fresh.cfg.ImageUpdateState()
	if active != paused {
		t.Fatal("maintenance undid explicit rollback")
	}
}

func TestMigrationFailuresKeepCurrentImage(t *testing.T) {
	for _, kind := range []string{"space", "probe", "settings", "persist", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s, b, _ := fixture(t)
			s.status.Images = nil
			switch kind {
			case "space":
				b.flattenErr = errors.New("insufficient space")
			case "probe":
				b.probeErr = errors.New("probe failed")
			case "settings":
				b.beforeProbe = func() {
					p, _, _ := s.cfg.ImageUpdateState()
					p.Enabled = false
					if err := s.cfg.ApplySettings(config.SettingsPatch{ImageUpdates: &p}); err != nil {
						t.Fatal(err)
					}
				}
			case "persist":
				b.beforeProbe = func() { s.path = filepath.Join(t.TempDir(), "missing", "state.json") }
			}
			job, err := s.Prepare("maintain", false)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "cancel" {
				cancel()
			}
			job(ctx)
			_, active, previous := s.cfg.ImageUpdateState()
			if active != "browser" || previous != "" || s.Snapshot().Error == "" {
				t.Fatal(s.Snapshot())
			}
		})
	}
}

func TestMaintenanceWithoutAutomaticVersionUpdates(t *testing.T) {
	s, b, requests := fixture(t)
	s.status.Images = nil
	p, _, _ := s.cfg.ImageUpdateState()
	p.Enabled = false
	if err := s.cfg.ApplySettings(config.SettingsPatch{ImageUpdates: &p}); err != nil {
		t.Fatal(err)
	}
	job, err := s.Prepare("maintain", true)
	if err != nil || job == nil {
		t.Fatal(err)
	}
	job(t.Context())
	if b.flattenCalls != 1 || *requests != 0 {
		t.Fatal("disabled updates changed versions")
	}
	if job, err = s.Prepare("maintain", true); err != nil || job != nil {
		t.Fatal("repeated daily maintenance")
	}
}

func TestInterruptedMigrationReusesStagedBase(t *testing.T) {
	s, b, _ := fixture(t)
	s.status.Images = nil
	b.probeErr = errors.New("interrupted validation")
	run(t, s, "maintain")
	fresh := New(s.cfg, b)
	fresh.registry = s.registry
	b.probeErr = nil
	run(t, fresh, "update")
	_, active, _ := fresh.cfg.ImageUpdateState()
	if active != "flat-id" || b.flattenCalls != 2 || b.probes != 2 {
		t.Fatal("failed to resume and revalidate")
	}
}

func TestGCProtectsCurrentRollbackBasesAndGracePeriod(t *testing.T) {
	s, b, _ := fixture(t)
	old := time.Now().Add(-8 * 24 * time.Hour).UnixMilli()
	s.status.Images = map[string]ImageRecord{
		"old-id":  {Base: "base-id", RetiredAt: old},
		"base-id": {Base: "base-id", RetiredAt: old},
		"retired": {Base: "base-id", RetiredAt: old},
		"recent":  {Base: "base-id", RetiredAt: time.Now().UnixMilli()},
	}
	b.images["base-id"] = Image{ID: "base-id"}
	b.removeOK = true
	if err := s.collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(b.removed) != 1 || b.removed[0] != "retired" {
		t.Fatalf("unsafe GC: %v", b.removed)
	}
	if _, ok := s.Snapshot().Images["retired"]; ok {
		t.Fatal("GC not persisted")
	}
}

func TestCorruptMaintenanceStateFailsClosed(t *testing.T) {
	s, b, _ := fixture(t)
	if err := os.WriteFile(s.path, []byte(`{"images":`), 0600); err != nil {
		t.Fatal(err)
	}
	fresh := New(s.cfg, b)
	if _, err := fresh.Prepare("update", false); err == nil {
		t.Fatal("discarded corrupt provenance")
	}
}

func TestMissingBaseNeverFallsBackToCurrent(t *testing.T) {
	s, b, _ := fixture(t)
	s.status.Images["old-id"] = ImageRecord{Base: "missing"}
	run(t, s, "update")
	_, active, _ := s.cfg.ImageUpdateState()
	if active != "browser" || b.builds != 0 || b.flattenCalls != 0 || s.Snapshot().Error == "" {
		t.Fatal(s.Snapshot())
	}
}

func TestRollbackPausesMigrationAcrossRestart(t *testing.T) {
	s, b, _ := fixture(t)
	s.status.Images = nil
	run(t, s, "maintain")
	run(t, s, "rollback")
	fresh := New(s.cfg, b)
	run(t, fresh, "maintain")
	_, active, _ := s.cfg.ImageUpdateState()
	if active != "old-id" || b.flattenCalls != 1 {
		t.Fatal("silently migrated rollback", fresh.Snapshot())
	}
}

func TestFailedMaintenanceBlocksScheduledUpdateButAllowsRetry(t *testing.T) {
	s, b, _ := fixture(t)
	s.status.Images = nil
	b.flattenErr = errors.New("space")
	run(t, s, "maintain")
	if job, err := s.Prepare("update", true); err != nil || job != nil {
		t.Fatal("repeated failed migration automatically")
	}
	b.flattenErr = nil
	run(t, s, "update")
	_, active, _ := s.cfg.ImageUpdateState()
	if active != "flat-id" || s.Snapshot().MaintenanceFailed != "" {
		t.Fatal(s.Snapshot())
	}
}
