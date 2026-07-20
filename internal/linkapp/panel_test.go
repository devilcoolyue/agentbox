package linkapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The panel can open a hole into the user's intranet, so the guard in front of
// it is load-bearing: loopback binding alone does not stop a hostile web page
// from POSTing to 127.0.0.1, nor DNS rebinding from reaching it by name.
func TestLocalGuard(t *testing.T) {
	handler := localGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name   string
		path   string
		host   string
		origin string
		panel  string
		want   int
	}{
		{"loopback api with header", "/api/state", "127.0.0.1:7801", "", "1", 200},
		{"localhost name", "/api/state", "localhost:7801", "", "1", 200},
		{"ipv6 loopback", "/api/state", "[::1]:7801", "", "1", 200},
		{"static asset needs no header", "/style.css", "127.0.0.1:7801", "", "", 200},
		{"api without header", "/api/state", "127.0.0.1:7801", "", "", 403},
		{"rebound hostname", "/api/state", "evil.example.com", "", "1", 403},
		{"public ip host", "/api/state", "203.0.113.5:7801", "", "1", 403},
		{"foreign origin", "/api/state", "127.0.0.1:7801", "https://evil.example.com", "1", 403},
		{"own origin", "/api/state", "127.0.0.1:7801", "http://127.0.0.1:7801", "1", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.panel != "" {
				req.Header.Set("X-Abox-Panel", tc.panel)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// The panel serves its whole view from one endpoint; the token must never be
// part of it, since anything the page can read a hostile script could exfiltrate.
func TestStateOmitsToken(t *testing.T) {
	p := NewPanel(Config{
		Server: "https://box.example.com", User: "alice", Token: "super-secret-token",
		Allow: []string{"10.0.0.0/8"},
	}, NewSupervisor())

	raw, err := json.Marshal(p.state())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-token") {
		t.Fatalf("state leaked the session token: %s", raw)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["paired"] != true || got["user"] != "alice" {
		t.Errorf("state = %v", got)
	}
}

func TestStartRefusesUnpairedConfig(t *testing.T) {
	sup := NewSupervisor()
	err := sup.Start(Config{Allow: []string{"10.0.0.0/8"}})
	if err == nil {
		t.Fatal("want an error when there are no server credentials")
	}
	if sup.Running() {
		t.Error("supervisor should not be running after a rejected start")
	}
}

func TestStartRefusesRulelessConfig(t *testing.T) {
	sup := NewSupervisor()
	err := sup.Start(Config{Server: "https://box.example.com", User: "a", Token: "t"})
	if err == nil {
		t.Fatal("want an error when there are no allow rules")
	}
}

// Stop must be safe to call on a supervisor that never started — the panel's
// stop button and the shutdown path both do it unconditionally.
func TestStopWhenIdleIsHarmless(t *testing.T) {
	NewSupervisor().Stop()
}

func TestCleanList(t *testing.T) {
	got := cleanList([]string{" 10.0.0.0/8 ", "", "   ", "db:5432"})
	if len(got) != 2 || got[0] != "10.0.0.0/8" || got[1] != "db:5432" {
		t.Fatalf("cleanList = %q", got)
	}
	if cleanList(nil) == nil {
		t.Error("cleanList should return an empty slice, not nil, so the UI never sees null")
	}
}

func TestLogRingKeepsLatestAndTracksSeq(t *testing.T) {
	r := newLogRing(3)
	for i := 0; i < 5; i++ {
		r.Printf("line %d", i)
	}
	lines, latest := r.Since(0)
	if len(lines) != 3 || lines[0].Text != "line 2" {
		t.Fatalf("ring did not retain the newest lines: %+v", lines)
	}
	if latest != 5 {
		t.Errorf("latest = %d, want 5", latest)
	}
	// A follow-up poll should return only what is new.
	newer, _ := r.Since(latest)
	if len(newer) != 0 {
		t.Errorf("want no lines after the latest seq, got %d", len(newer))
	}
	r.Printf("line 5")
	newer, _ = r.Since(latest)
	if len(newer) != 1 || newer[0].Text != "line 5" {
		t.Errorf("incremental poll = %+v", newer)
	}
}
