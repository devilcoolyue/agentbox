package dockerx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
	"github.com/docker/docker/client"
)

func TestMissingAgentImageHasTypedErrorAndDoesNotCreateContainer(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1.45/images/synthetic-agent/json" {
			t.Error("missing image caused unexpected Docker mutation", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"message":"No such image: synthetic-private-image"}`))
	}))
	defer api.Close()
	cli, err := client.NewClientWithOpts(client.WithHost(api.URL), client.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	m := &Manager{cli: cli, cfg: &config.Config{AgentImage: "synthetic-agent"}}
	_, err = m.EnsureRunning(t.Context(), store.Session{ID: "fixture"}, config.Account{}, "/synthetic/workspace", "/synthetic/home", "/synthetic/shared")
	if !errors.Is(err, ErrImageMissing) {
		t.Fatalf("not a typed image failure: %v", err)
	}
}
