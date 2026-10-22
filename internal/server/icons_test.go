package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIconRoutesPermissionsAndShapes(t *testing.T) {
	s, _ := accessTestServer(t)
	for _, user := range []string{"alice", "root"} {
		if err := s.store.CreateToken("icons-"+user, user); err != nil {
			t.Fatal(err)
		}
	}
	handler := s.Handler()
	request := func(user, method, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/api/icons", strings.NewReader(body))
		if user != "" {
			r.Header.Set("Authorization", "Bearer icons-"+user)
		}
		handler.ServeHTTP(w, r)
		return w
	}
	var got struct {
		Overrides map[string]string            `json:"overrides"`
		Icons     map[string][]json.RawMessage `json:"icons"`
		UpdatedBy string                       `json:"updated_by"`
	}
	read := func(w *httptest.ResponseRecorder) {
		t.Helper()
		got.Overrides, got.Icons = nil, nil
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
	}

	// 登录页也要按管理员的配置画图标，读取不需要登录
	if w := request("", "GET", ""); w.Code != 200 {
		t.Fatalf("anonymous read: %d %s", w.Code, w.Body.String())
	} else if read(w); len(got.Overrides) != 0 {
		t.Fatalf("fresh config: %+v", got)
	}
	body := `{"overrides":{"nav.usage":"tabler:receipt-2","close":"lucide:x"}}`
	for _, tc := range []struct {
		user, body string
		status     int
	}{
		{"", body, 401},
		{"alice", body, 403},
		{"root", `{"overrides":{"nav.usage":"tabler:does-not-exist"}}`, 400},
		{"root", `{"overrides":{"nav.usage":"<svg onload=x>"}}`, 400},
		{"root", `not json`, 400},
	} {
		if w := request(tc.user, "PUT", tc.body); w.Code != tc.status {
			t.Fatalf("%q %s: %d %s", tc.user, tc.body, w.Code, w.Body.String())
		}
	}
	w := request("root", "PUT", body)
	if w.Code != 200 {
		t.Fatalf("admin save: %d %s", w.Code, w.Body.String())
	}
	read(w)
	if got.UpdatedBy != "root" || len(got.Icons["tabler:receipt-2"]) == 0 || len(got.Icons["lucide:x"]) == 0 || len(got.Icons) != 2 {
		t.Fatalf("saved response: %+v", got)
	}
	read(request("alice", "GET", ""))
	if got.Overrides["nav.usage"] != "tabler:receipt-2" || len(got.Icons) != 2 {
		t.Fatalf("read back: %+v", got)
	}
	if w := request("root", "PUT", `{"overrides":{}}`); w.Code != 200 {
		t.Fatalf("reset: %d", w.Code)
	}
	if read(request("", "GET", "")); len(got.Overrides) != 0 {
		t.Fatalf("reset not applied: %+v", got)
	}
}
