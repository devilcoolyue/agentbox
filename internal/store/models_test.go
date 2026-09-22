package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceDefaultModelSurvivesSettingsAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	for _, sess := range []Session{
		{ID: "legacy", Agent: "claude", CreatedAt: time.Now()},
		{ID: "first", Agent: "codex", DefaultModel: "gpt-5.5", CreatedAt: time.Now()},
	} {
		if err := s.Put(sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InitDefaultModels(map[string]string{"claude": "claude-opus-5", "codex": "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	// The admin changes defaults, then creates another workspace.
	if err := s.Put(Session{ID: "next", Agent: "codex", DefaultModel: "gpt-5.5-codex", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update("first", func(sess *Session) { sess.Name = "renamed" }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InitDefaultModels(map[string]string{"claude": "claude-sonnet-5", "codex": "gpt-5.5-codex"}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"legacy": "claude-opus-5", "first": "gpt-5.5", "next": "gpt-5.5-codex"} {
		got, ok := s.Get(id)
		if !ok || got.DefaultModel != want {
			t.Errorf("workspace %s model = %q, want %q", id, got.DefaultModel, want)
		}
	}
}
