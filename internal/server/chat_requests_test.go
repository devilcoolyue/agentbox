package server

import (
	"path/filepath"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func TestStartupRecoversChatReceiptsBeforeDocker(t *testing.T) {
	data := t.TempDir()
	path := filepath.Join(data, "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.CreateUser(store.User{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	u, _ := st.GetUser("alice")
	if err = st.Put(store.Session{ID: "space", User: u.Name}); err != nil {
		t.Fatal(err)
	}
	input := store.ChatRequestInput{Scope: "scope", ThreadID: "thread", Text: "synthetic request"}
	c, _, err := st.AcceptChatRequest(u, "space", "request", input)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	// Force Docker construction to fail without touching a real daemon. Recovery
	// must already be durable, even when the rest of startup cannot complete.
	t.Setenv("DOCKER_HOST", "not-a-docker-url")
	server, err := NewContext(t.Context(), &config.Config{DataDir: data})
	if err == nil {
		closeTestServer(t, server)
		t.Fatal("invalid Docker host unexpectedly accepted")
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := reopened.ChatRequest(u, "space", c.RequestID)
	if err != nil || recovered.State != store.ChatUncertain || recovered.TurnID != c.TurnID || recovered.ErrorCode != "server_restarted" {
		t.Fatal("startup left accepted request available to rerun", recovered, err)
	}
}
