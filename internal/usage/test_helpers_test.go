package usage

import (
	"context"
	"path/filepath"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

type fixtureService struct {
	*Service
	store *store.Store
}

func newTestServer(t *testing.T) (*fixtureService, store.Session) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &fixtureService{New(context.Background(), &config.Config{DataDir: root}, st), st}, store.Session{ID: "s1", User: "alice", Name: "fixture"}
}
