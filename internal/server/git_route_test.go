package server

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/gitaccess"
	"agentbox/internal/store"
	"agentbox/internal/tunnel"
)

func TestGitCompanyCAAndUserTunnel(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, "git fixture") }))
	defer upstream.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}))
	s, _ := newTestServer(t)
	s.tunnels = newTunnelHub()
	s.cfg.Tunnel = config.TunnelConfig{Enabled: true}
	c := store.GitConnection{Owner: "alice", Network: gitaccess.NetworkPolicy{CAPEM: ca}}
	client, closeClient, err := s.gitClient(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	closeClient()
	c.Network.CAPEM = ""
	client, closeClient, err = s.gitClient(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err = client.Get(upstream.URL); err == nil {
		resp.Body.Close()
		t.Fatal("untrusted CA accepted")
	}
	closeClient()
	c.Network = gitaccess.NetworkPolicy{Route: "tunnel", CAPEM: ca}
	before := hits.Load()
	client, closeClient, err = s.gitClient(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err = client.Get(upstream.URL); err == nil {
		resp.Body.Close()
		t.Fatal("offline tunnel fell back to direct")
	}
	closeClient()
	if hits.Load() != before {
		t.Fatal("offline tunnel reached server")
	}
	target := strings.TrimPrefix(upstream.URL, "https://")
	allow, err := tunnel.ParseWhitelist([]string{target})
	if err != nil {
		t.Fatal(err)
	}
	wireLink(t, s.tunnels, "bob", allow)
	waitReady(t, s.tunnels, "bob")
	if _, err = s.gitDial(context.Background(), c, "tcp", target); err == nil {
		t.Fatal("used another user's tunnel")
	}
	shared := c
	shared.Owner = "bob"
	shared.Actor = "alice"
	if _, err = s.gitDial(t.Context(), shared, "tcp", target); err == nil {
		t.Fatal("shared credential borrowed owner's tunnel")
	}
	wireLink(t, s.tunnels, "alice", allow)
	waitReady(t, s.tunnels, "alice")
	client, closeClient, err = s.gitClient(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	closeClient()
	if hits.Load() != before+1 {
		t.Fatal("user tunnel did not reach target")
	}
	if _, err = s.gitDial(t.Context(), c, "tcp", "not-allowed.invalid:443"); err == nil {
		t.Fatal("tunnel ignored allowlist")
	}
	s.cfg.Tunnel.Enabled = false
	if _, err = s.gitDial(t.Context(), c, "tcp", target); err == nil {
		t.Fatal("disabled tunnel used")
	}
}
