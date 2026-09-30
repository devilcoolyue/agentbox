package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"agentbox/internal/store"
)

func TestImageUpdateEndpointsAdminOnly(t *testing.T) {
	s, _ := newTestServer(t)
	for _, role := range []string{store.RoleUser, store.RoleAdmin} {
		if err := s.store.CreateUser(store.User{Name: role, Role: role, PassHash: "unused"}); err != nil {
			t.Fatal(err)
		}
		if err := s.store.CreateToken(role+"-token", role); err != nil {
			t.Fatal(err)
		}
	}
	handler := s.Handler()
	for _, path := range []string{"/api/image-updates", "/api/image-updates/check", "/api/image-updates/update", "/api/image-updates/rollback"} {
		for _, role := range []string{"", store.RoleUser} {
			method := "POST"
			if path == "/api/image-updates" {
				method = "GET"
			}
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Authorization", "Bearer "+role+"-token")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			want := 403
			if role == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatalf("%s %s: %d", role, path, w.Code)
			}
		}
	}
	req := httptest.NewRequest("GET", "/api/image-updates", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("admin status: %d", w.Code)
	}
	// Reserve a job without starting Docker/network work; every action must reject
	// concurrent admission and shutdown must be able to finish the reservation.
	job, err := s.imageUpdater().Prepare("check", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"check", "update", "rollback"} {
		req := httptest.NewRequest("POST", "/api/image-updates/"+action, nil)
		req.Header.Set("Authorization", "Bearer admin-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 409 {
			t.Fatalf("concurrent %s: %d", action, w.Code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job(ctx)
	if s.imageUpdater().Snapshot().Running {
		t.Fatal("canceled job left running")
	}
}
