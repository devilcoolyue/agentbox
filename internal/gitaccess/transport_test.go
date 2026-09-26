package gitaccess

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTransportLimitsCredentialsAndPushTarget(t *testing.T) {
	calls := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		u, p, ok := r.BasicAuth()
		if !ok || u != "fixture" || p != "synthetic-secret" {
			t.Error("missing upstream authentication")
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("browser cookie leaked")
		}
		service := "git-upload-pack"
		if strings.Contains(r.URL.String(), "receive-pack") {
			service = "git-receive-pack"
		}
		suffix := "-result"
		if r.Method == "GET" {
			suffix = "-advertisement"
		}
		w.Header().Set("Content-Type", "application/x-"+service+suffix)
		fmt.Fprint(w, "fixture payload")
	}))
	defer upstream.Close()
	grant := Grant{Ticket: "fixture-ticket", Repository: upstream.URL + "/repo.git", Client: upstream.Client(), Authorize: func(context.Context) (string, string, error) { return "fixture", "synthetic-secret", nil }}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Cookie", "should-not-leak")
		grant.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/fixture-ticket/info/refs?service=git-upload-pack", ""); w.Code != 200 {
		t.Fatalf("read: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/bad/info/refs?service=git-upload-pack", "/fixture-ticket/info/refs?service=git-receive-pack", "/fixture-ticket/../other", "/fixture-ticket/info/refs?service=git-upload-pack&extra=1"} {
		if w := request("GET", path, ""); w.Code == 200 {
			t.Fatalf("accepted %s", path)
		}
	}
	if calls != 1 {
		t.Fatalf("denied requests reached upstream: %d", calls)
	}
	grant.Write = true
	grant.Old = strings.Repeat("0", 40)
	grant.New = strings.Repeat("a", 40)
	grant.Ref = "refs/heads/feature"
	command := grant.Old + " " + grant.New + " " + grant.Ref + "\x00report-status side-band-64k\n"
	packet := fmt.Sprintf("%04x%s0000PACK", len(command)+4, command)
	if w := request("POST", "/fixture-ticket/git-receive-pack", packet); w.Code != 200 {
		t.Fatalf("push: %d %s", w.Code, w.Body.String())
	}
	bad := strings.Replace(packet, "refs/heads/feature", "refs/heads/another", 1)
	if w := request("POST", "/fixture-ticket/git-receive-pack", bad); w.Code != 409 {
		t.Fatalf("wrong branch accepted: %d", w.Code)
	}
	if w := request("POST", "/fixture-ticket/git-receive-pack", strings.Replace(packet, "0000PACK", "0008more0000PACK", 1)); w.Code != 409 {
		t.Fatalf("multiple commands accepted: %d", w.Code)
	}
	if calls != 2 {
		t.Fatalf("denied pushes reached upstream: %d", calls)
	}
}
func TestTransportNeverFollowsRedirects(t *testing.T) {
	calls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; io.WriteString(w, "wrong") }))
	defer destination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer upstream.Close()
	g := Grant{Ticket: "ticket", Repository: upstream.URL, Client: upstream.Client(), Authorize: func(context.Context) (string, string, error) { return "u", "secret", nil }}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "/ticket/info/refs?service=git-upload-pack", nil))
	if calls != 0 || w.Code != 302 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("redirect leaked: %d %d %s", calls, w.Code, w.Body.String())
	}
}
