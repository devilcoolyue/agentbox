package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/netaccess"
)

func TestNetworkPolicyPersistsRetiredRoutes(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.saveNetworkRules("alice", []string{"db.corp:5432", "10.21.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.networkPolicy("alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.saveNetworkRules("alice", []string{"new.corp"}); err != nil {
		t.Fatal(err)
	}
	s.network.policies = nil
	after, err := s.networkPolicy("alice")
	if err != nil {
		t.Fatal(err)
	}
	if before.Domains["db.corp"] != after.Domains["db.corp"] || after.Allows("db.corp:5432") || !after.Captures("10.21.0.1") {
		t.Fatal(after)
	}
	bob, _ := s.networkPolicy("bob")
	if bob.Captures("10.21.0.1") {
		t.Fatal("cross-user route")
	}
}
func TestNetworkControlScopesAndRevokesCredentials(t *testing.T) {
	s, sess := accessTestServer(t)
	sess.AccountID = "shared"
	sess.ContainerID = "container-one"
	s.store.Put(sess)
	other := sess
	other.ID = "s2"
	other.User = "bob"
	s.store.Put(other)
	if err := s.saveNetworkRules("alice", []string{"alice.corp"}); err != nil {
		t.Fatal(err)
	}
	if err := s.saveNetworkRules("bob", []string{"bob.corp"}); err != nil {
		t.Fatal(err)
	}
	get := func(id, secret string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/policy", nil)
		r.SetBasicAuth(id, secret)
		w := httptest.NewRecorder()
		s.handleNetworkControl(w, r)
		return w
	}
	if w := get(sess.ID, s.networkSecret(sess)); w.Code != 200 || strings.Contains(w.Body.String(), "bob.corp") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := get(other.ID, s.networkSecret(sess)); w.Code != 401 {
		t.Fatal("cross-user credential accepted")
	}
	old := s.networkSecret(sess)
	sess.ContainerID = "container-two"
	s.store.Put(sess)
	if w := get(sess.ID, old); w.Code != 401 {
		t.Fatal("replaced container credential accepted")
	}
	sess.AccountID = "selected"
	s.store.Put(sess)
	if w := get(sess.ID, s.networkSecret(sess)); w.Code != 403 {
		t.Fatal("revoked account allowed")
	}
}
func TestNetworkDialCannotFallThrough(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.Tunnel = config.TunnelConfig{Enabled: true, Transparent: true}
	s.tunnels = newTunnelHub()
	if err := s.saveNetworkRules("alice", []string{"10.21.1.1:80"}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"10.21.1.1:80", "10.21.1.1:22", "example.com:80"} {
		if c, err := s.dialIntranet(context.Background(), "alice", target); err == nil {
			c.Close()
			t.Fatalf("offline/unauthorized target accepted: %s", target)
		}
	}
}
func TestNetworkStatusRequiresFreshCurrentContainer(t *testing.T) {
	s, sess := newTestServer(t)
	sess.ContainerID = "new"
	p, _ := netaccess.Merge(netaccess.Policy{}, nil)
	s.network.reports = map[string]networkReport{sess.ID: {Report: netaccess.Report{Revision: p.Revision}, Container: "old"}}
	if s.networkStatus(sess, p).Ready {
		t.Fatal("stale ready status")
	}
}

func TestDisabledTunnelDoesNotPrepareNetwork(t *testing.T) {
	s, sess := newTestServer(t)
	s.cfg.Tunnel = config.TunnelConfig{Transparent: true, Enabled: false}
	if err := s.ensureNetwork(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if s.network.ln != nil {
		t.Fatal("disabled tunnel must not start listener or helper")
	}
}
