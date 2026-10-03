package syncclient

import "testing"

func TestStateLifecycleUpgradeAndArchivedStateSurviveRestart(t *testing.T) {
	s, saved, directory := stateFixture(t)
	// A version-1 DB has the same tables and no archive flags. Upgrade must retain
	// its device and mapping, while setting a version old sidecars will refuse.
	device, err := s.Device()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenState(directory)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
		t.Fatal(version, err)
	}
	if current, err := s.Device(); err != nil || current != device {
		t.Fatal("device changed", err)
	}
	archived, err := s.change(saved.ID, saved.Revision, func(saved *SavedBinding) error { saved.Archived = true; return nil }, false)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenState(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, err := s.Load(saved.ID)
	if err != nil || !loaded.Archived || loaded.Revision != archived.Revision {
		t.Fatal("archive lost on restart", err)
	}
	fresh, err := s.Register(saved.Binding, saved.Directory)
	if err != nil || fresh.ID == saved.ID {
		t.Fatal(err)
	}
	if _, err = s.Begin(loaded.ID, loaded.Revision, 1, Plan{}, "", tree(nil), tree(nil), PlanOptions{}); err != ErrBinding {
		t.Fatal("archived state accepted new batch", err)
	}
}

func TestStateVersionTwoUpgradePreservesPending(t *testing.T) {
	s, saved, directory := stateFixture(t)
	pending := beginFixture(t, s, saved)
	device, err := s.Device()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := OpenState(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Load(saved.ID)
	if err != nil || got.Pending.ID != pending.Pending.ID || got.Revision != pending.Revision {
		t.Fatal("pending changed during migration", err)
	}
	if got, err := reopened.Device(); err != nil || got != device {
		t.Fatal("device changed", err)
	}
	var version int
	if err = reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
		t.Fatal(version, err)
	}
}
