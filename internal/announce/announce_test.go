package announce

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func TestNormalizeRejectsBadInput(t *testing.T) {
	for name, in := range map[string]Input{
		"empty notice":   {Kind: KindNotice, Message: "  "},
		"unknown kind":   {Kind: "banner", Message: "x"},
		"too long":       {Kind: KindNotice, Message: strings.Repeat("字", MaxMessageRunes+1)},
		"past start":     {Kind: KindRelease, StartsAt: t0.Add(-2 * time.Hour).UnixMilli()},
		"far start":      {Kind: KindRelease, StartsAt: t0.Add(MaxLead + time.Hour).UnixMilli()},
		"long downtime":  {Kind: KindRelease, DurationMin: MaxDurationMin + 1},
		"negative delay": {Kind: KindRelease, DurationMin: -1},
	} {
		var ve *ValidationError
		if _, err := in.Normalize(t0); !errors.As(err, &ve) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	got, err := Input{Kind: KindNotice, Message: " hi\r\nthere ", StartsAt: 5, DurationMin: 3}.Normalize(t0)
	if err != nil || got.Message != "hi\nthere" || got.StartsAt != 0 || got.DurationMin != 0 {
		t.Fatalf("notice normalize = %+v, %v", got, err)
	}
	if _, err = (Input{Kind: KindRelease}).Normalize(t0); err != nil {
		t.Fatalf("release without time or text rejected: %v", err)
	}
}

func TestPutGetClearRevision(t *testing.T) {
	s := New(t.TempDir())
	if a, err := s.Get(); a != nil || err != nil {
		t.Fatalf("empty Get = %v, %v", a, err)
	}
	if rev, _ := s.Revision(); rev != "" {
		t.Fatalf("empty revision = %q", rev)
	}
	a, err := s.Put(Input{Kind: KindRelease, Message: "升级", StartsAt: t0.Add(5 * time.Minute).UnixMilli(), DurationMin: 2}, "boxadmin", "v1", t0)
	if err != nil {
		t.Fatal(err)
	}
	rev1, _ := s.Revision()
	got, err := s.Get()
	if err != nil || got.ID != a.ID || got.FromVersion != "v1" || got.PostedBy != "boxadmin" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	b, _ := s.Put(Input{Kind: KindNotice, Message: "x"}, "boxadmin", "v1", t0)
	if b.ID == a.ID || b.FromVersion != "" {
		t.Fatalf("replacement kept ID or version: %+v", b)
	}
	if rev2, _ := s.Revision(); rev2 == rev1 || rev2 == "" {
		t.Fatal("revision did not change")
	}
	if err = s.Clear(); err != nil {
		t.Fatal(err)
	}
	if err = s.Clear(); err != nil {
		t.Fatalf("clearing twice: %v", err)
	}
	if rev, _ := s.Revision(); rev != "" {
		t.Fatal("revision after clear")
	}
}

func TestCorruptFileReported(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, file), []byte(`{"version":2}`), 0o600)
	if a, err := New(dir).Get(); a != nil || err == nil {
		t.Fatalf("corrupt Get = %v, %v", a, err)
	}
}

func TestCompleteOnUpgradeOnly(t *testing.T) {
	s := New(t.TempDir())
	s.Put(Input{Kind: KindRelease, StartsAt: t0.Add(10 * time.Minute).UnixMilli()}, "boxadmin", "v1", t0)
	// Restart before posting: not this release.
	if done, _ := s.Complete("v2", t0.Add(-time.Minute)); done {
		t.Fatal("completed by a start before posting")
	}
	// Plain restart on the same version.
	if done, _ := s.Complete("v1", t0.Add(time.Minute)); done {
		t.Fatal("completed without a version change")
	}
	started := t0.Add(2 * time.Minute)
	if done, err := s.Complete("v2", started); !done || err != nil {
		t.Fatalf("upgrade not completed: %v", err)
	}
	a, _ := s.Get()
	if a.CompletedVersion != "v2" || a.CompletedAt != started.UnixMilli() {
		t.Fatalf("completed = %+v", a)
	}
	if done, _ := s.Complete("v3", t0.Add(time.Hour)); done {
		t.Fatal("completed twice")
	}
	if !a.Visible(started.Add(CompletedVisible-time.Second)) || a.Visible(started.Add(CompletedVisible)) {
		t.Fatal("completed banner window wrong")
	}
}

// Without a known old version (posted by hand), only a start after the
// scheduled time counts as the release.
func TestCompleteWithoutFromVersionWaitsForSchedule(t *testing.T) {
	s := New(t.TempDir())
	start := t0.Add(10 * time.Minute)
	s.Put(Input{Kind: KindRelease, StartsAt: start.UnixMilli()}, "cli", "", t0)
	if done, _ := s.Complete("v2", t0.Add(time.Minute)); done {
		t.Fatal("completed before the schedule")
	}
	if done, _ := s.Complete("v2", start.Add(time.Second)); !done {
		t.Fatal("not completed after the schedule")
	}
}

func TestNoticeNeverCompletesAndReleaseGoesStale(t *testing.T) {
	s := New(t.TempDir())
	n, _ := s.Put(Input{Kind: KindNotice, Message: "hello"}, "boxadmin", "v1", t0)
	if done, _ := s.Complete("v2", t0.Add(time.Hour)); done {
		t.Fatal("notice completed")
	}
	if !n.Visible(t0.Add(365 * 24 * time.Hour)) {
		t.Fatal("notice expired by itself")
	}
	r := &Announcement{Kind: KindRelease, PostedAt: t0.UnixMilli()}
	if !r.Visible(t0.Add(StaleAfter-time.Second)) || r.Visible(t0.Add(StaleAfter)) {
		t.Fatal("stale release window wrong")
	}
	var none *Announcement
	if none.Visible(t0) {
		t.Fatal("nil visible")
	}
}
