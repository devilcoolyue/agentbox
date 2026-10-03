package syncfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"agentbox/internal/syncproto"
)

func fixtureRoot(t *testing.T) (*Root, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, dir
}
func TestScanHashesBytesAndDetectsSameSizeSameMtimeEdit(t *testing.T) {
	r, dir := fixtureRoot(t)
	rules, _ := syncproto.ParseRules("")
	data := []byte("a\r\nb\x00")
	file := filepath.Join(dir, "binary")
	if err := os.WriteFile(file, data, 0644); err != nil {
		t.Fatal(err)
	}
	first, err := r.Scan(t.Context(), rules, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Entries["binary"].Hash != syncproto.HashBytes(data) {
		t.Fatal("bytes transformed")
	}
	info, _ := os.Stat(file)
	data[0] = 'z'
	if err = os.WriteFile(file, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	second, err := r.Scan(t.Context(), rules, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Entries["binary"].Hash == first.Entries["binary"].Hash {
		t.Fatal("stat-only cache hid modification")
	}
	if _, err = r.Scan(t.Context(), rules, Limits{MaxFileBytes: 2}); !errors.Is(err, syncproto.ErrLimit) {
		t.Fatal("size not bounded", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = r.Scan(ctx, rules, Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestScanRejectsLinksAndNeverReturnsPartialManifest(t *testing.T) {
	r, dir := fixtureRoot(t)
	rules, _ := syncproto.ParseRules("")
	if err := os.WriteFile(filepath.Join(dir, "good"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "good"), filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	m, err := r.Scan(t.Context(), rules, Limits{})
	if err == nil || m.Entries != nil {
		t.Fatal("hardlinks accepted or partial manifest returned")
	}
	if err = os.Remove(filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(dir, filepath.Join(dir, "link")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("Windows symlink creation requires privilege; hardlink rejection already checked")
		}
		t.Fatal(err)
	}
	m, err = r.Scan(t.Context(), rules, Limits{})
	if err == nil || m.Entries != nil {
		t.Fatal("symlink accepted")
	}
	if _, err = r.OpenFile("link/good"); err == nil {
		t.Fatal("followed directory link")
	}
}

func TestMissingAndReplacedRootAreErrorsNotEmptyTrees(t *testing.T) {
	r, dir := fixtureRoot(t)
	rules, _ := syncproto.ParseRules("")
	if _, err := Open(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing root created")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows may deny renaming an open root; covered by platform integration later")
	}
	if err := os.Rename(dir, dir+"-moved"); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir + "-moved")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := r.Scan(t.Context(), rules, Limits{})
	if !errors.Is(err, ErrRootChanged) || m.Entries != nil {
		t.Fatalf("replacement tree accepted: %+v %v", m, err)
	}
}

func TestProbePreservesUserFilesAndStableIdentity(t *testing.T) {
	r, dir := fixtureRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := r.Probe()
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Probe()
	if err != nil {
		t.Fatal(err)
	}
	if a.DirectoryID != b.DirectoryID || a.NamePolicy != b.NamePolicy || !syncproto.ValidHash(a.DirectoryID) {
		t.Fatalf("unstable probe: %+v %+v", a, b)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "keep" {
		t.Fatal("probe changed user files or left temporary files", entries, err)
	}
}

func TestDestinationNamesWindowsCaseAndUnicodeCollision(t *testing.T) {
	rules, _ := syncproto.ParseRules("")
	m := syncproto.Manifest{Version: 1, RulesHash: rules.Hash(), Entries: map[string]syncproto.Entry{}}
	for _, name := range []string{"A.ts", "a.ts", "é.ts", "e\u0301.ts", "CON.txt", "COM¹.txt", "x:stream", "trailing.", "space "} {
		m.Entries[name] = syncproto.Entry{Kind: "file", Hash: syncproto.HashBytes(nil)}
	}
	issues, err := CheckNames(m, NamePolicy{Windows: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 7 {
		t.Fatalf("issues: %+v", issues)
	}
	issues, err = CheckNames(m, NamePolicy{CaseSensitive: true, NormalizationSensitive: true})
	if err != nil || len(issues) != 0 {
		t.Fatalf("Linux names unnecessarily rejected: %+v %v", issues, err)
	}
}

func TestIgnoredNamesStillCountTowardEnumerationLimit(t *testing.T) {
	r, dir := fixtureRoot(t)
	for _, name := range []string{"a.log", "b.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	rules, err := syncproto.ParseRules("*.log")
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.Scan(t.Context(), rules, Limits{MaxEntries: 1})
	if !errors.Is(err, syncproto.ErrLimit) || m.Entries != nil {
		t.Fatalf("ignored enumeration bypassed limit: %+v %v", m, err)
	}
}
