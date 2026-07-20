package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPairCodeIsSingleUse(t *testing.T) {
	p := newPairStore()
	code, ok := p.issue("alice")
	if !ok {
		t.Fatal("issue failed")
	}
	if user, ok := p.redeem(code); !ok || user != "alice" {
		t.Fatalf("first redeem = %q, %v", user, ok)
	}
	if _, ok := p.redeem(code); ok {
		t.Fatal("a pairing code must not be redeemable twice")
	}
}

func TestPairCodeExpires(t *testing.T) {
	p := newPairStore()
	code, _ := p.issue("alice")
	p.mu.Lock()
	e := p.codes[code]
	e.expires = time.Now().Add(-time.Second)
	p.codes[code] = e
	p.mu.Unlock()

	if _, ok := p.redeem(code); ok {
		t.Fatal("an expired code must not redeem")
	}
}

// Clicking "generate" again should not leave the previous code live — the user
// reasonably assumes the one on screen is the only one that works.
func TestReissueInvalidatesPreviousCode(t *testing.T) {
	p := newPairStore()
	first, _ := p.issue("alice")
	second, _ := p.issue("alice")

	if _, ok := p.redeem(first); ok {
		t.Error("the superseded code should no longer redeem")
	}
	if _, ok := p.redeem(second); !ok {
		t.Error("the newest code should redeem")
	}
}

func TestReissueDoesNotDisturbOtherUsers(t *testing.T) {
	p := newPairStore()
	bobs, _ := p.issue("bob")
	p.issue("alice")
	p.issue("alice")

	if user, ok := p.redeem(bobs); !ok || user != "bob" {
		t.Fatalf("bob's code was collateral damage: %q %v", user, ok)
	}
}

func TestRedeemUnknownCode(t *testing.T) {
	p := newPairStore()
	p.issue("alice")
	if _, ok := p.redeem("not-a-real-code"); ok {
		t.Fatal("unknown code redeemed")
	}
}

func TestPairOrigin(t *testing.T) {
	cases := []struct {
		name    string
		claimed string
		req     func() *http.Request
		want    string
	}{
		{
			name:    "browser origin wins",
			claimed: "https://box.example.com",
			req:     func() *http.Request { return httptest.NewRequest("POST", "/api/tunnel/pair", nil) },
			want:    "https://box.example.com",
		},
		{
			name:    "falls back to request host",
			claimed: "",
			req: func() *http.Request {
				r := httptest.NewRequest("POST", "/api/tunnel/pair", nil)
				r.Host = "box.internal:8080"
				return r
			},
			want: "http://box.internal:8080",
		},
		{
			name:    "honours a terminating proxy's headers",
			claimed: "",
			req: func() *http.Request {
				r := httptest.NewRequest("POST", "/api/tunnel/pair", nil)
				r.Host = "127.0.0.1:8080"
				r.Header.Set("X-Forwarded-Host", "box.example.com, inner")
				r.Header.Set("X-Forwarded-Proto", "https")
				return r
			},
			want: "https://box.example.com",
		},
		{
			name:    "ignores a nonsense claim",
			claimed: "javascript:alert(1)",
			req: func() *http.Request {
				r := httptest.NewRequest("POST", "/api/tunnel/pair", nil)
				r.Host = "box.internal"
				return r
			},
			want: "http://box.internal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pairOrigin(tc.claimed, tc.req()); got != tc.want {
				t.Errorf("pairOrigin = %q, want %q", got, tc.want)
			}
		})
	}
}
