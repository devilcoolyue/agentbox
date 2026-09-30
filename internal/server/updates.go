package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"agentbox/internal/buildinfo"
)

const releasesURL = "https://github.com/devilcoolyue/agentbox/releases"
const latestReleaseURL = "https://api.github.com/repos/devilcoolyue/agentbox/releases/latest"
const updateInterval = 4 * time.Hour

// Only public release metadata is returned; this API never installs a binary.
type updateInfo struct {
	CurrentVersion string `json:"current_version"`
	Revision       string `json:"revision"`
	BuiltAt        string `json:"built_at"`
	LatestVersion  string `json:"latest_version"`
	Available      bool   `json:"available"`
	Comparable     bool   `json:"comparable"`
	ReleaseURL     string `json:"release_url"`
	Notes          string `json:"notes"`
	CheckedAt      int64  `json:"checked_at"`
	AttemptedAt    int64  `json:"attempted_at"`
	Error          string `json:"error"`
}

type updateState struct {
	mu   sync.Mutex
	info updateInfo
}

var releaseVersionRE = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)

// Development builds may switch to any latest stable release, even when their
// numeric core is ahead. Comparable only describes ordered release versions.
func newerStableRelease(current, latest string) (available, comparable bool) {
	c, l := releaseVersionRE.FindStringSubmatch(current), releaseVersionRE.FindStringSubmatch(latest)
	if l == nil || l[4] != "" || strings.Contains(latest, "dirty") {
		return false, false
	}
	if current == "dev" || (c != nil && (c[4] != "" || strings.Contains(current, "dirty"))) {
		return true, false
	}
	if c == nil {
		return false, false
	}
	for i := 1; i <= 3; i++ {
		if len(c[i]) != len(l[i]) {
			return len(l[i]) > len(c[i]), true
		}
		if c[i] != l[i] {
			return l[i] > c[i], true
		}
	}
	return false, true
}

func (u *updateState) snapshot() updateInfo {
	info := u.info
	info.CurrentVersion, info.Revision, info.BuiltAt = buildinfo.Version, buildinfo.Commit(), buildinfo.BuiltAt
	current := releaseVersionRE.FindStringSubmatch(buildinfo.Version)
	info.Comparable = current != nil && current[4] == "" && !strings.Contains(buildinfo.Version, "dirty")
	if info.LatestVersion != "" {
		info.Available, info.Comparable = newerStableRelease(info.CurrentVersion, info.LatestVersion)
	}
	// Release builds mark uncommitted sources in Revision, not Version.
	if strings.Contains(info.Revision, "+dirty") {
		info.Comparable = false
		if current != nil {
			info.Available, _ = newerStableRelease("dev", info.LatestVersion)
		}
	}
	if info.ReleaseURL == "" {
		info.ReleaseURL = releasesURL
	}
	return info
}

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	s.updates.mu.Lock()
	info := s.updates.snapshot()
	s.updates.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	info := s.updates.check(r.Context(), http.DefaultClient, latestReleaseURL, r.URL.Query().Get("force") == "1")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, info)
}

// Shared server cache coalesces all browser tabs. Even manual checks have a
// one-minute cooldown so repeated clicks cannot exhaust GitHub's API quota.
func (u *updateState) check(ctx context.Context, client *http.Client, endpoint string, force bool) updateInfo {
	u.mu.Lock()
	defer u.mu.Unlock()
	interval := updateInterval
	if force {
		interval = time.Minute
	}
	if u.info.AttemptedAt != 0 && time.Since(time.UnixMilli(u.info.AttemptedAt)) < interval {
		return u.snapshot()
	}
	if ctx.Err() != nil {
		return u.snapshot()
	}
	u.info.AttemptedAt = time.Now().UnixMilli()
	latest, notes, err := fetchLatestRelease(ctx, client, endpoint)
	if err != nil {
		// Keep the last successful release visible, but never call a failed
		// check "up to date". Network details aren't exposed to the browser.
		u.info.Error = "暂时无法获取发布信息，请稍后重试（手动检查间隔 1 分钟）。"
		return u.snapshot()
	}
	u.info.LatestVersion, u.info.Notes, u.info.Error = latest, notes, ""
	u.info.ReleaseURL = releasesURL
	if latest != "" {
		u.info.ReleaseURL += "/tag/" + url.PathEscape(latest)
	}
	u.info.CheckedAt = time.Now().UnixMilli()
	return u.snapshot()
}

func fetchLatestRelease(ctx context.Context, client *http.Client, endpoint string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "agentbox-update-check")
	res, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return "", "", nil // No public stable release yet.
	}
	if res.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("release status %d", res.StatusCode)
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Body       string `json:"body"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&release); err != nil {
		return "", "", err
	}
	version := releaseVersionRE.FindStringSubmatch(release.Tag)
	if version == nil || version[4] != "" || release.Draft || release.Prerelease {
		return "", "", fmt.Errorf("invalid stable release")
	}
	notes := []rune(release.Body)
	if len(notes) > 12000 {
		notes = append(notes[:12000], []rune("\n…完整说明请查看发布页。")...)
	}
	return release.Tag, string(notes), nil
}
