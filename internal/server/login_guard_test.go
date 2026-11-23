package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentbox/internal/store"
)

func loginFrom(s *Server, addr, user, pw string) int {
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"`+user+`","password":"`+pw+`"}`))
	r.RemoteAddr = addr
	w := httptest.NewRecorder()
	s.handleLogin(w, r)
	return w.Code
}

// A concurrent burst must not get more password checks than the limit:
// verification is slow, so attempts counted only after failing all got in.
func TestLoginRateLimitHoldsUnderConcurrency(t *testing.T) {
	s, _ := newTestServer(t)
	const n = 4 * loginMaxFails
	codes := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = loginFrom(s, "203.0.113.9:4444", "nobody", "wrong")
		}()
	}
	close(start)
	wg.Wait()
	checked := 0
	for _, c := range codes {
		switch c {
		case http.StatusUnauthorized:
			checked++
		case http.StatusTooManyRequests:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if checked != loginMaxFails {
		t.Fatalf("%d of %d concurrent attempts reached password verification, want %d", checked, n, loginMaxFails)
	}
}

func TestLoginSuccessClearsCountedAttempts(t *testing.T) {
	s, _ := newTestServer(t)
	old := loginFailDelay
	loginFailDelay = 0
	t.Cleanup(func() { loginFailDelay = old })
	if err := s.store.CreateUser(store.User{Name: "bob", Role: store.RoleUser, PassHash: hashPassword("right-pw")}); err != nil {
		t.Fatal(err)
	}
	const addr = "203.0.113.10:4444"
	for round := 0; round < 2; round++ {
		for i := 0; i < loginMaxFails-1; i++ {
			if c := loginFrom(s, addr, "bob", "wrong"); c != http.StatusUnauthorized {
				t.Fatalf("round %d fail %d: %d", round, i, c)
			}
		}
		if c := loginFrom(s, addr, "bob", "right-pw"); c != http.StatusOK {
			t.Fatalf("round %d: correct login got %d", round, c)
		}
	}
	for i := 0; i < loginMaxFails; i++ {
		loginFrom(s, addr, "bob", "wrong")
	}
	if c := loginFrom(s, addr, "bob", "right-pw"); c != http.StatusTooManyRequests {
		t.Fatalf("locked key got %d", c)
	}
	// Another address is unaffected.
	if c := loginFrom(s, "203.0.113.11:4444", "bob", "right-pw"); c != http.StatusOK {
		t.Fatalf("other address got %d", c)
	}
}
