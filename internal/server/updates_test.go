package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/buildinfo"
	"agentbox/internal/store"
)

func TestUpdateSnapshotDirtyRevision(t *testing.T) {
	version, revision := buildinfo.Version, buildinfo.Revision
	t.Cleanup(func() { buildinfo.Version, buildinfo.Revision = version, revision })
	buildinfo.Version, buildinfo.Revision = "v0.1.0-rc.2", "synthetic+dirty"
	state := updateState{info: updateInfo{LatestVersion: "v0.1.0"}}
	info := state.snapshot()
	if !info.Available || info.Comparable {
		t.Fatalf("uncommitted build cannot switch to stable: %+v", info)
	}
	buildinfo.Revision = "synthetic"
	info = state.snapshot()
	if !info.Available || info.Comparable {
		t.Fatalf("release candidate should offer a switch to stable: %+v", info)
	}
}

func TestNewerStableRelease(t *testing.T) {
	for _, tc := range []struct {
		current, latest       string
		available, comparable bool
	}{
		{"v0.9.9", "v0.10.0", true, true},
		{"1.2.3", "v1.2.3", false, true},
		{"v2.0.0", "v1.99.99", false, true},
		{"v1.0.0-rc.1", "v1.0.0", true, false},
		{"v1.1.0-rc.1", "v1.0.0", true, false},
		{"v1.0.0+build.123", "v1.0.0", false, true},
		{"dev", "v1.0.0", true, false},
		{"dev", "v1.0.0-rc.1", false, false},
		{"dev", "", false, false},
		{"dev", "v1.0.0+dirty", false, false},
		{"v1.0.0+dirty", "v1.0.1", true, false},
		{"v1.0.0-3-gabcdef", "v1.0.1", true, false},
		{"v01.0.0", "v1.0.0", false, false},
		{"v1.0.0", "v2.0.0-rc.1", false, false},
		{"v1.0.0", "garbage", false, false},
		{"v99999999999999999999.0.0", "v100000000000000000000.0.0", true, true},
	} {
		t.Run(tc.current+"_"+tc.latest, func(t *testing.T) {
			a, c := newerStableRelease(tc.current, tc.latest)
			if a != tc.available || c != tc.comparable {
				t.Fatalf("got %v %v, want %v %v", a, c, tc.available, tc.comparable)
			}
		})
	}
}

func TestDirtyStableSnapshotOffersSameOrOlderStable(t *testing.T) {
	version, revision := buildinfo.Version, buildinfo.Revision
	t.Cleanup(func() { buildinfo.Version, buildinfo.Revision = version, revision })
	buildinfo.Version, buildinfo.Revision = "v1.1.0", "fixture+dirty"
	for _, latest := range []string{"v1.1.0", "v1.0.0", ""} {
		state := updateState{info: updateInfo{LatestVersion: latest}}
		info := state.snapshot()
		if info.Comparable || info.Available != (latest != "") {
			t.Fatalf("unexpected development transition: %+v", info)
		}
	}
}

func TestFetchLatestRelease(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body, latest string
		fail         bool
	}{
		{"release", 200, `{"tag_name":"v1.2.3","body":"<script>plain text</script>","html_url":"https://evil.invalid/"}`, "v1.2.3", false},
		{"none", 404, "", "", false},
		{"rate limit", 403, "", "", true},
		{"upstream error", 502, "", "", true},
		{"bad JSON", 200, "<html>", "", true},
		{"bad tag", 200, `{"tag_name":"../../other"}`, "", true},
		{"draft", 200, `{"tag_name":"v1.0.0","draft":true}`, "", true},
		{"prerelease", 200, `{"tag_name":"v1.0.0","prerelease":true}`, "", true},
		{"rc", 200, `{"tag_name":"v1.0.0-rc.1"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("credentials sent to upstream")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			latest, _, err := fetchLatestRelease(t.Context(), upstream.Client(), upstream.URL)
			if latest != tc.latest || (err != nil) != tc.fail {
				t.Fatalf("got %q, %v", latest, err)
			}
		})
	}
}

func TestUpdateCacheCoalescesAndPreservesLastSuccess(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(502)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v1.2.3", "body": "Release notes", "html_url": "https://evil.invalid"})
	}))
	defer upstream.Close()
	var state updateState
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { state.check(t.Context(), upstream.Client(), upstream.URL, false) })
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent checks made %d calls", calls.Load())
	}
	first := state.check(t.Context(), upstream.Client(), upstream.URL, true)
	if calls.Load() != 1 || first.CheckedAt == 0 || first.Error != "" || first.ReleaseURL != releasesURL+"/tag/v1.2.3" {
		t.Fatalf("unexpected snapshot: %+v", first)
	}
	state.info.AttemptedAt = time.Now().Add(-2 * time.Minute).UnixMilli()
	state.check(t.Context(), upstream.Client(), upstream.URL, false)
	if calls.Load() != 1 {
		t.Fatal("automatic check ignored four-hour cache")
	}
	fail.Store(true)
	last := state.check(t.Context(), upstream.Client(), upstream.URL, true)
	if calls.Load() != 2 || last.Error == "" || last.LatestVersion != first.LatestVersion || last.CheckedAt != first.CheckedAt || last.Notes != first.Notes {
		t.Fatalf("failed check lost previous result: %+v", last)
	}
	state.info.AttemptedAt = time.Now().Add(-5 * time.Hour).UnixMilli()
	fail.Store(false)
	last = state.check(t.Context(), upstream.Client(), upstream.URL, false)
	if calls.Load() != 3 || last.Error != "" {
		t.Fatalf("didn't recover: %+v", last)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	state.info.AttemptedAt = 0
	state.check(ctx, upstream.Client(), upstream.URL, true)
	if calls.Load() != 3 {
		t.Fatal("cancelled check contacted upstream")
	}
}

func TestUpdateEndpointsRequireAdmin(t *testing.T) {
	s, _ := newTestServer(t)
	for _, role := range []string{store.RoleUser, store.RoleAdmin} {
		if err := s.store.CreateUser(store.User{Name: role, Role: role, PassHash: "unused"}); err != nil {
			t.Fatal(err)
		}
		if err := s.store.CreateToken(role+"-token", role); err != nil {
			t.Fatal(err)
		}
	}
	s.updates.info.AttemptedAt = time.Now().UnixMilli() // Never contact a real release server.
	handler := s.Handler()
	for _, path := range []string{"/api/updates", "/api/updates/check", "/api/updates/upgrade"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "/check") {
			method = http.MethodPost
		}
		for _, role := range []string{"", store.RoleUser, store.RoleAdmin} {
			req := httptest.NewRequest(method, path, nil)
			if role != "" {
				req.Header.Set("Authorization", "Bearer "+role+"-token")
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			want := http.StatusOK
			if role == "" {
				want = http.StatusUnauthorized
			} else if role == store.RoleUser {
				want = http.StatusForbidden
			}
			if w.Code != want {
				t.Fatalf("%s %s: got %d want %d", role, path, w.Code, want)
			}
		}
	}
	// A query token cannot authorize the write endpoint.
	req := httptest.NewRequest(http.MethodPost, "/api/updates/check?token=admin-token", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("query token accepted: %d", w.Code)
	}
	for _, token := range []string{"", "user-token", "query"} {
		path := "/api/updates/upgrade"
		if token == "query" {
			path += "?token=admin-token"
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"version":"v1.2.3"}`))
		if token == "user-token" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		want := http.StatusUnauthorized
		if token == "user-token" {
			want = http.StatusForbidden
		}
		if w.Code != want {
			t.Fatalf("upgrade %s: got %d want %d", token, w.Code, want)
		}
	}
}
