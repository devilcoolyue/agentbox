package server

import (
	"net/http/httptest"
	"testing"
)

// Only the hop the reverse proxy appended counts; a client-supplied prefix
// must not change the throttling key.
func TestClientIPIgnoresForgedForwardedFor(t *testing.T) {
	for _, c := range []struct {
		remote string
		xff    []string
		want   string
	}{
		{"127.0.0.1:5000", []string{"1.2.3.4, 203.0.113.9"}, "203.0.113.9"},
		{"127.0.0.1:5000", []string{"9.9.9.9", "203.0.113.9"}, "203.0.113.9"},
		{"127.0.0.1:5000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"[::1]:5000", []string{"2001:db8::7"}, "2001:db8::7"},
		{"10.0.0.5:5000", []string{"::ffff:203.0.113.9"}, "203.0.113.9"},
		{"127.0.0.1:5000", nil, "127.0.0.1"},
		{"127.0.0.1:5000", []string{"203.0.113.9, garbage"}, "127.0.0.1"},
		// A client talking to agentbox directly cannot pick its own key.
		{"198.51.100.20:5000", []string{"1.2.3.4"}, "198.51.100.20"},
	} {
		r := httptest.NewRequest("POST", "/api/login", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("remote=%s xff=%q: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}
