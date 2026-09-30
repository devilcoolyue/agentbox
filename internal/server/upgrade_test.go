package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbox/internal/buildinfo"
)

func TestUpgradeHelperRequiresCurrentVersionedInstallation(t *testing.T) {
	for _, version := range []string{"v1.0.0", "dev", "v2.0.0-dev.1"} {
		t.Run(version, func(t *testing.T) {
			app := t.TempDir()
			pkg := filepath.Join(app, "releases", version)
			if err := os.MkdirAll(filepath.Join(pkg, "deploy"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"agentbox", "deploy/update.py", "deploy/release.py"} {
				if err := os.WriteFile(filepath.Join(pkg, name), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			exe := filepath.Join(pkg, "agentbox")
			if _, _, err := upgradeHelper(exe, version); err == nil {
				t.Fatal("accepted missing current link")
			}
			if err := os.Symlink(pkg, filepath.Join(app, "current")); err != nil {
				t.Fatal(err)
			}
			got, helper, err := upgradeHelper(exe, version)
			resolved, _ := filepath.EvalSymlinks(app)
			if err != nil || got != resolved || helper != filepath.Join(resolved, "releases", version, "deploy/update.py") {
				t.Fatalf("%s %s %v", got, helper, err)
			}
			if _, _, err := upgradeHelper(exe, "v2.0.0"); err == nil {
				t.Fatal("accepted wrong version")
			}
			if err := os.Remove(filepath.Join(pkg, "deploy/update.py")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("release.py", filepath.Join(pkg, "deploy/update.py")); err != nil {
				t.Fatal(err)
			}
			if _, _, err := upgradeHelper(exe, version); err == nil {
				t.Fatal("accepted symlink helper")
			}
		})
	}
}

func TestUpgradeStartRejectsUnreviewedVersions(t *testing.T) {
	version, revision := buildinfo.Version, buildinfo.Revision
	t.Cleanup(func() { buildinfo.Version, buildinfo.Revision = version, revision })
	buildinfo.Version, buildinfo.Revision = "v1.0.0", "fixture"
	s, _ := newTestServer(t)
	s.updates.info.LatestVersion = "v1.1.0"
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"version":"../../bad"}`, 400},
		{`{"version":"v1.1.0","url":"https://evil.invalid"}`, 400},
		{`{"version":"v1.1.0"} {}`, 400},
		{`{"version":"v1.0.0"}`, 409},
		{`{"version":"v2.0.0"}`, 409},
		{`{"version":"v1.1.0"}`, 409}, // Test executable is never a supported installation.
	} {
		w := httptest.NewRecorder()
		s.handleUpgradeStart(w, httptest.NewRequest(http.MethodPost, "/api/updates/upgrade", strings.NewReader(tc.body)))
		if w.Code != tc.want {
			t.Fatalf("%s: got %d, want %d", tc.body, w.Code, tc.want)
		}
	}
	s.updates.info.Error = "upstream unavailable"
	w := httptest.NewRecorder()
	s.handleUpgradeStart(w, httptest.NewRequest(http.MethodPost, "/api/updates/upgrade", strings.NewReader(`{"version":"v1.1.0"}`)))
	if w.Code != http.StatusConflict {
		t.Fatal("accepted failed release check")
	}
}

func TestDevelopmentUpgradeStartAcceptsReviewedStableTarget(t *testing.T) {
	version, revision := buildinfo.Version, buildinfo.Revision
	t.Cleanup(func() { buildinfo.Version, buildinfo.Revision = version, revision })
	s, _ := newTestServer(t)
	s.updates.info.LatestVersion = "v1.0.0"
	for _, current := range []string{"dev", "v2.0.0-dev.1", "v1.0.0", "v2.0.0"} {
		buildinfo.Version, buildinfo.Revision = current, "fixture+dirty"
		w := httptest.NewRecorder()
		s.handleUpgradeStart(w, httptest.NewRequest(http.MethodPost, "/api/updates/upgrade", strings.NewReader(`{"version":"v1.0.0"}`)))
		// The test executable is not installed, but version admission must pass.
		if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "版本检查结果") {
			t.Fatalf("%s: version admission failed: %d %s", current, w.Code, w.Body.String())
		}
	}
}
