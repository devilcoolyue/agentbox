package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/config"
)

func TestDataLockAndInitializationFailureRelease(t *testing.T) {
	dir := t.TempDir()
	first, err := lockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockDataDir(dir); err == nil {
		second.Close()
		t.Fatal("second owner acquired lock")
	}
	first.Close()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "synthetic initialization failure", 500) }))
	defer daemon.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(daemon.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.45")
	cfg := &config.Config{DataDir: dir}
	if err := Run(context.Background(), cfg); err == nil {
		t.Fatal("unreachable daemon accepted")
	}
	next, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("initialization leaked data lock: %v", err)
	}
	next.Close()
}
