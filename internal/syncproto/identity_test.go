package syncproto

import "testing"

func TestNormalizeServer(t *testing.T) {
	for input, want := range map[string]string{"https://EXAMPLE.com:443/base//": "https://example.com/base/", "http://[::1]:80": "http://[::1]/", "https://example.com:8443/": "https://example.com:8443/"} {
		got, err := NormalizeServer(input)
		if err != nil || got != want {
			t.Fatalf("%s: %s %v", input, got, err)
		}
	}
	for _, input := range []string{"https://user:secret@host/", "https://host/?token=x", "https://host/#x", "file:///tmp/x", "https://host/%2e%2e/x", "https://host/?"} {
		if _, err := NormalizeServer(input); err == nil {
			t.Fatal(input)
		}
	}
}
