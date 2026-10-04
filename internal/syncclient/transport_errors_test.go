package syncclient

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agentbox/internal/syncproto"
)

func TestTransportFailureKeepsPermanentErrorsAndCancellationOutOfRetry(t *testing.T) {
	for _, tt := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"read interrupted", fmt.Errorf("private address: %w", io.ErrUnexpectedEOF), true},
		{"connection closed", io.EOF, true},
		{"temporary DNS", &net.DNSError{Err: "private hostname", IsTemporary: true}, true},
		{"missing DNS", &net.DNSError{Err: "private hostname", IsNotFound: true}, false},
		{"timeout", &net.DNSError{Err: "private hostname", IsTimeout: true}, true},
		{"unknown certificate", x509.UnknownAuthorityError{}, false},
		{"wrong hostname", x509.HostnameError{Certificate: &x509.Certificate{}, Host: "private hostname"}, false},
		{"canceled", context.Canceled, false},
		{"unknown", errors.New("private address: unknown error"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := transportFailure(tt.err)
			if !errors.Is(err, ErrTransport) || transientPreviewFailure(t.Context(), err) != tt.retry {
				t.Fatalf("classification: %v", err)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("transport details escaped sanitization")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if transientPreviewFailure(ctx, errTransientTransport) || transientPreviewFailure(t.Context(), context.DeadlineExceeded) {
		t.Fatal("canceled request may not retry")
	}
}

func TestDesktopOnlyPreviewClassifiesTemporaryIdentityFailures(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504, 301, 400, 401, 403, 404, 409, 413, 501} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
			}))
			defer server.Close()
			for _, kind := range []string{"sync_preview", "sync_apply", "sync_resolve", "sync_orphan_retire"} {
				request := command{Version: 1, ID: "request", Type: kind, Server: server.URL, User: "alice", Token: "secret-token", StateDir: filepath.Join(t.TempDir(), "must-not-exist")}
				result := syncEvent(t.Context(), request)
				want := "sync_identity"
				if kind == "sync_preview" && (status == 408 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504) {
					want = "sync_preview_retryable"
				}
				if result.Type != "error" || result.Error != want {
					t.Fatalf("%s: got %+v, want %s", kind, result, want)
				}
				if _, err := os.Stat(request.StateDir); !os.IsNotExist(err) {
					t.Fatal("failed authentication touched local state")
				}
			}
			if calls.Load() != 4 {
				t.Fatal("transport itself retried requests", calls.Load())
			}
		})
	}
}

func TestRemoteRetryClassificationUsesActualConnectionAndBodyFailures(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted TLS reached handler") }))
	defer secure.Close()
	for _, tt := range []struct {
		name, address string
		retry         bool
	}{
		{"connection refused", closed.URL, true}, {"untrusted TLS", secure.URL, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			remote, err := NewRemote(tt.address, "secret-token", false)
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			for _, call := range []func() error{
				func() error { _, e := remote.Identity(t.Context()); return e },
				func() error { _, e := remote.Manifest(t.Context(), "space", "project"); return e },
			} {
				err = call()
				if !errors.Is(err, ErrTransport) || transientPreviewFailure(t.Context(), err) != tt.retry {
					t.Fatal("unexpected classification", err)
				}
			}
		})
	}
	for _, truncated := range []bool{true, false} {
		t.Run(fmt.Sprintf("truncated=%v", truncated), func(t *testing.T) {
			remote := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if truncated {
					w.Header().Set("Content-Length", "100")
				}
				_, _ = io.WriteString(w, `{"version":`)
			})
			_, err := remote.Identity(t.Context())
			if transientPreviewFailure(t.Context(), err) != truncated {
				t.Fatal("malformed JSON must not become a network retry", err)
			}
		})
	}
}

func TestDesktopTransientPreviewPreservesBindingAndReturnsToReadOnlyPlan(t *testing.T) {
	var unavailable atomic.Bool
	serverID := syncproto.HashBytes([]byte("retry-fixture"))
	rules, _ := syncproto.ParseRules("")
	manifest := syncproto.Manifest{Version: 1, RulesHash: rules.Hash(), Entries: map[string]syncproto.Entry{}}
	digest, _ := manifest.Digest()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("preview attempted a write")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Agentbox-Server-ID", serverID)
		switch r.URL.Path {
		case "/api/clients/capabilities":
			_ = json.NewEncoder(w).Encode(syncproto.ServerIdentity{Version: 1, ServerID: serverID, User: "alice", Features: map[string]int{"sync": 1}})
		case "/api/sessions/space/sync/manifest":
			if unavailable.Load() {
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(syncproto.ManifestResponse{Manifest: manifest, Digest: digest, Project: "project", Revision: 1, ProjectPath: "."})
		default:
			t.Error("unexpected request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private = filepath.Join(private, "state")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	request := command{Version: 1, ID: "bind", Type: "sync_bind", Server: server.URL, User: "alice", Token: "secret-token", StateDir: private, Directory: local,
		Binding: &SyncBinding{Workspace: "space", Project: "project", ProjectPath: "."}}
	bound := syncEvent(t.Context(), request)
	if bound.Type != "sync_bound" || bound.Binding == nil {
		t.Fatalf("bind: %+v", bound)
	}
	request.Type = "sync_preview"
	request.BindingID = bound.Binding.ID
	unavailable.Store(true)
	failed := syncEvent(t.Context(), request)
	if failed.Error != "sync_preview_retryable" || failed.Preview != nil {
		t.Fatalf("preview: %+v", failed)
	}
	unavailable.Store(false)
	recovered := syncEvent(t.Context(), request)
	if recovered.Type != "sync_previewed" || recovered.Preview == nil || recovered.Preview.StateRevision != bound.Binding.Revision {
		t.Fatalf("preview changed durable state: %+v", recovered)
	}
	request.Type = "sync_list"
	listed := syncEvent(t.Context(), request)
	if len(listed.Bindings) != 1 || listed.Bindings[0].Pending || listed.Bindings[0].Revision != bound.Binding.Revision {
		t.Fatalf("preview created pending write: %+v", listed)
	}
}
