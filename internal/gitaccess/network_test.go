package gitaccess

import (
	"encoding/pem"
	"net/http/httptest"
	"testing"
)

func TestGitNetworkPolicyValidation(t *testing.T) {
	for _, p := range []NetworkPolicy{{Route: "invalid"}, {CAPEM: "not a certificate"}, {CAPEM: "-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----"}} {
		if p.Validate() == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	p := NetworkPolicy{Route: "tunnel", CAPEM: ca}
	cfg, err := p.TLSConfig()
	if err != nil || cfg.InsecureSkipVerify || cfg.RootCAs == nil {
		t.Fatalf("TLS trust: %+v %v", cfg, err)
	}
	p.CAPEM += "\nprivate-key-text"
	if p.Validate() == nil {
		t.Fatal("trailing private material accepted")
	}
}
