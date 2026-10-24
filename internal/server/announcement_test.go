package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbox/internal/announce"
)

func announcementTestServer(t *testing.T) (*Server, func(user, method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	s, _ := accessTestServer(t)
	s.usageLive = newUsageLiveHub()
	for _, user := range []string{"alice", "root"} {
		if err := s.store.CreateToken("announce-"+user, user); err != nil {
			t.Fatal(err)
		}
	}
	handler := s.Handler()
	return s, func(user, method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if user != "" {
			r.Header.Set("Authorization", "Bearer announce-"+user)
		}
		handler.ServeHTTP(w, r)
		return w
	}
}

func decodeAnnouncement(t *testing.T, w *httptest.ResponseRecorder) announcementView {
	t.Helper()
	var v announcementView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%d %s: %v", w.Code, w.Body.String(), err)
	}
	return v
}

func TestAnnouncementRoutesPermissionsAndPush(t *testing.T) {
	s, request := announcementTestServer(t)
	page := s.usageLive.subscribe("alice", false)

	if w := request("", "GET", "/api/announcement", ""); w.Code != 401 {
		t.Fatalf("anonymous read: %d", w.Code)
	}
	if v := decodeAnnouncement(t, request("alice", "GET", "/api/announcement", "")); v.Announcement != nil || v.Now == 0 {
		t.Fatalf("empty read: %+v", v)
	}
	start := time.Now().Add(10 * time.Minute).UnixMilli()
	body := `{"kind":"release","message":"今晚升级","starts_at":` + jsonInt(start) + `,"duration_min":2}`
	for _, tc := range []struct {
		user, method, path, body string
		status                   int
	}{
		{"", "PUT", "/api/announcement", body, 401},
		{"alice", "PUT", "/api/announcement", body, 403},
		{"alice", "DELETE", "/api/announcement", "", 403},
		{"alice", "GET", "/api/announcement/activity", "", 403},
		{"root", "PUT", "/api/announcement", `{"kind":"notice","message":"  "}`, 400},
		{"root", "PUT", "/api/announcement", `{"kind":"banner","message":"x"}`, 400},
		{"root", "PUT", "/api/announcement", `not json`, 400},
	} {
		if w := request(tc.user, tc.method, tc.path, tc.body); w.Code != tc.status {
			t.Fatalf("%q %s %s: %d %s", tc.user, tc.method, tc.body, w.Code, w.Body.String())
		}
	}
	if len(page.signal) != 0 {
		t.Fatal("rejected writes pushed a change")
	}
	w := request("root", "PUT", "/api/announcement", body)
	if w.Code != 200 {
		t.Fatalf("admin post: %d %s", w.Code, w.Body.String())
	}
	if _, _, announced := page.take(); !announced {
		t.Fatal("open page not told about the new announcement")
	}
	v := decodeAnnouncement(t, request("alice", "GET", "/api/announcement", ""))
	a := v.Announcement
	if a == nil || a.Kind != announce.KindRelease || a.Message != "今晚升级" || a.StartsAt != start || a.DurationMin != 2 || a.PostedBy != "root" || a.ID == "" {
		t.Fatalf("read back: %+v", a)
	}
	if w := request("root", "DELETE", "/api/announcement", ""); w.Code != 200 || decodeAnnouncement(t, w).Announcement != nil {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if _, _, announced := page.take(); !announced {
		t.Fatal("open page not told about the withdrawal")
	}
	if v := decodeAnnouncement(t, request("alice", "GET", "/api/announcement", "")); v.Announcement != nil {
		t.Fatalf("withdrawn announcement still served: %+v", v.Announcement)
	}
}

func jsonInt(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// An upgraded banner disappears by itself; a broken file does not break pages.
func TestAnnouncementHidesExpiredAndCorrupt(t *testing.T) {
	s, request := announcementTestServer(t)
	svc := s.announcements()
	posted := time.Now().Add(-time.Hour)
	if _, err := svc.Put(announce.Input{Kind: announce.KindRelease}, "root", "v-old", posted); err != nil {
		t.Fatal(err)
	}
	if done, err := svc.Complete("v-new", posted.Add(time.Minute)); !done || err != nil {
		t.Fatalf("complete: %v %v", done, err)
	}
	if v := decodeAnnouncement(t, request("alice", "GET", "/api/announcement", "")); v.Announcement != nil {
		t.Fatalf("expired completion still served: %+v", v.Announcement)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "announcement.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := request("alice", "GET", "/api/announcement", ""); w.Code != 200 || decodeAnnouncement(t, w).Announcement != nil {
		t.Fatalf("corrupt file: %d %s", w.Code, w.Body.String())
	}
}

func TestAnnouncementActivityListsAffectedUse(t *testing.T) {
	s, request := announcementTestServer(t)
	s.usageLive.subscribe("alice", false)
	s.usageLive.subscribe("alice", false)
	s.usageLive.subscribe("root", true)
	sessions := s.store.All()
	if len(sessions) == 0 {
		t.Fatal("fixture has no session")
	}
	busy := sessions[0]
	s.workspaces().Activity().Hold(busy.ID)
	w := request("root", "GET", "/api/announcement/activity", "")
	if w.Code != 200 {
		t.Fatalf("activity: %d %s", w.Code, w.Body.String())
	}
	var got announcementActivity
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Online) != 2 || got.Online[0].User != "alice" || got.Online[0].Pages != 2 || got.Online[1].User != "root" {
		t.Fatalf("online = %+v", got.Online)
	}
	if len(got.Workspaces) != 1 || got.Workspaces[0].ID != busy.ID || got.Workspaces[0].Attached != 1 || got.Workspaces[0].User != busy.User {
		t.Fatalf("workspaces = %+v", got.Workspaces)
	}
}
