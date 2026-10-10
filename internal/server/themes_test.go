package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const themeFixture = `{"agentbox_theme":1,"id":"%ID%","name":"Fixture","base":"verdant","dark":{"--bg":"#101820"},"light":{"--bg":"#f4f8f4"}}`

func TestThemeRoutesScopeAndPermissions(t *testing.T) {
	s, _ := accessTestServer(t)
	for _, user := range []string{"alice", "bob", "root"} {
		if err := s.store.CreateToken("theme-"+user, user); err != nil {
			t.Fatal(err)
		}
	}
	handler := s.Handler()
	request := func(user, method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer theme-"+user)
		handler.ServeHTTP(w, r)
		return w
	}
	fixture := func(id string) string { return strings.ReplaceAll(themeFixture, "%ID%", id) }

	for _, tc := range []struct {
		user, method, path, body string
		status                   int
	}{
		{"alice", "PUT", "/api/themes/site/shared", fixture("shared"), 403},
		{"alice", "DELETE", "/api/themes/site/shared", "", 403},
		{"missing", "GET", "/api/themes", "", 401},
		{"root", "PUT", "/api/themes/site/shared", fixture("shared"), 200},
		{"alice", "PUT", "/api/themes/user/mine", fixture("mine"), 200},
		{"alice", "PUT", "/api/themes/user/other", fixture("mine"), 400},
		{"alice", "PUT", "/api/themes/user/Bad_ID", fixture("mine"), 400},
		{"bob", "DELETE", "/api/themes/user/mine", "", 404},
	} {
		if w := request(tc.user, tc.method, tc.path, tc.body); w.Code != tc.status {
			t.Fatalf("%s %s %s: %d %s", tc.user, tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	var list struct {
		Site []themeView `json:"site"`
		User []themeView `json:"user"`
		Can  bool        `json:"can_manage_site"`
		Toks []struct {
			Name string `json:"name"`
		} `json:"tokens"`
	}
	decode := func(w *httptest.ResponseRecorder) {
		t.Helper()
		list.Site, list.User = nil, nil
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	decode(request("alice", "GET", "/api/themes", ""))
	if len(list.Site) != 1 || list.Site[0].Scope != "site" || len(list.User) != 1 || list.User[0].Manifest.ID != "mine" || list.Can || len(list.Toks) == 0 {
		t.Fatalf("alice view: %+v", list)
	}
	decode(request("bob", "GET", "/api/themes", ""))
	if len(list.Site) != 1 || len(list.User) != 0 {
		t.Fatalf("bob sees alice's personal theme: %+v", list)
	}
	decode(request("root", "GET", "/api/themes", ""))
	if !list.Can {
		t.Fatal("admin cannot manage site themes")
	}

	// Validation failures carry a stable code for the browser to translate.
	w := request("alice", "PUT", "/api/themes/user/evil", `{"agentbox_theme":1,"id":"evil","name":"E","base":"amber","dark":{"--bg":"url(https://evil.example)"}}`)
	var problem struct {
		Error string `json:"error"`
		Theme struct {
			Code, Token, Section string
		} `json:"theme_error"`
	}
	if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &problem) != nil || problem.Theme.Code != "bad_value" || problem.Theme.Token != "--bg" || problem.Theme.Section != "dark" || problem.Error == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	// Validation without saving: same verdicts as PUT, nothing written.
	if w := request("bob", "POST", "/api/themes/validate", `{"agentbox_theme":1,"id":"draft","name":"  Draft ","base":"amber","dark":{"--bg":"#000"}}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"Draft"`) {
		t.Fatal("validate", w.Code, w.Body.String())
	}
	if w := request("bob", "POST", "/api/themes/validate", `{"agentbox_theme":1,"id":"draft","name":"D","base":"amber","dark":{"--bg":"url(x)"}}`); w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"bad_value"`) {
		t.Fatal("validate bad value", w.Code, w.Body.String())
	}
	if w := request("missing", "POST", "/api/themes/validate", `{}`); w.Code != 401 {
		t.Fatal("validate requires login", w.Code)
	}
	decode(request("bob", "GET", "/api/themes", ""))
	if len(list.User) != 0 {
		t.Fatal("validate saved a theme", list.User)
	}
	if w := request("alice", "PUT", "/api/themes/user/big", `{"agentbox_theme":1,"id":"big","name":"`+strings.Repeat("a", 70<<10)+`"}`); w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), `"code":"too_large"`) || !strings.Contains(w.Body.String(), "64 KiB") {
		t.Fatal("oversized body", w.Code, w.Body.String())
	}

	// Deleting a user drops their personal themes; a same-name account starts clean.
	if w := request("root", "DELETE", "/api/users/alice", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "users", "alice", "themes.json")); !os.IsNotExist(err) {
		t.Fatal("personal themes survived user deletion", err)
	}
	if w := request("root", "DELETE", "/api/themes/site/shared", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	decode(request("bob", "GET", "/api/themes", ""))
	if len(list.Site) != 0 {
		t.Fatal("site theme not deleted", list.Site)
	}
}
