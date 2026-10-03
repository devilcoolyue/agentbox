package syncclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/syncfs"
	"agentbox/internal/syncproto"
)

func remoteFileRequest() syncproto.FileRequest {
	rules, _ := syncproto.ParseRules("")
	return syncproto.FileRequest{Project: "abc", Revision: 1, RulesHash: rules.Hash(), Path: "file", Expected: syncproto.Entry{Kind: "file", Hash: syncproto.HashBytes([]byte("new-data")), Size: 8}}
}
func serveFileHeaders(w http.ResponseWriter, req syncproto.FileRequest) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(req.Expected.Size, 10))
	w.Header().Set("ETag", `"`+req.Expected.Hash+`"`)
}
func newTestRemote(t *testing.T, handler http.HandlerFunc) *Remote {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	remote, err := NewRemote(server.URL, "private-fixture-token", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	return remote
}
func TestRemoteBadTransfersLeaveLocalFileIntact(t *testing.T) {
	for _, kind := range []string{"hash", "truncated", "etag", "type", "size", "encoding", "stale", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			request := remoteFileRequest()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sent := make(chan struct{})
			remote := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(sent)
				if r.Header.Get("Authorization") != "Bearer private-fixture-token" || strings.Contains(r.URL.String(), "private-fixture-token") {
					t.Error("credential boundary")
				}
				if kind == "stale" {
					w.WriteHeader(409)
					return
				}
				serveFileHeaders(w, request)
				switch kind {
				case "etag":
					w.Header().Set("ETag", `"wrong"`)
				case "type":
					w.Header().Set("Content-Type", "text/html")
				case "size":
					w.Header().Set("Content-Length", "9")
				case "encoding":
					w.Header().Set("Content-Encoding", "gzip")
				}
				switch kind {
				case "hash":
					_, _ = io.WriteString(w, "bad-data")
				case "truncated":
					_, _ = io.WriteString(w, "new")
				case "cancel":
					_, _ = io.WriteString(w, "new")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				default:
					_, _ = io.WriteString(w, "new-data")
				}
			})
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root, err := syncfs.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			file := filepath.Join(dir, "file")
			if err = os.WriteFile(file, []byte("original"), 0644); err != nil {
				t.Fatal(err)
			}
			before := syncproto.Entry{Kind: "file", Hash: syncproto.HashBytes([]byte("original")), Size: 8}
			stream, err := remote.File(ctx, "s1", request)
			if err == nil {
				if kind == "cancel" {
					cancel()
				}
				writer := syncfs.Writer{Root: root}
				_, err = writer.Replace(ctx, "file", &before, request.Expected, stream)
				stream.Close()
			}
			if err == nil {
				t.Fatal("invalid transfer accepted")
			}
			cancel()
			<-sent
			content, readErr := os.ReadFile(file)
			if readErr != nil || string(content) != "original" {
				t.Fatalf("destination changed: %q %v", content, readErr)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatal("failed download left staging/recovery", entries, err)
			}
		})
	}
}

func TestRemoteRejectsRedirectsTLSAndUnsafeAddresses(t *testing.T) {
	for _, address := range []string{"file:///tmp", "https://user:pass@example.com", "https://example.com?token=x", "https://example.com?", "https://example.com/#x", "https://example.com/%2f", "https://example.com/a\\b", "http://example.com"} {
		if remote, err := NewRemote(address, "token", false); err == nil {
			remote.Close()
			t.Fatalf("accepted %q", address)
		}
	}
	for _, token := range []string{"", "secret\nheader", "space token"} {
		if remote, err := NewRemote("https://example.com", token, false); err == nil {
			remote.Close()
			t.Fatal("invalid token")
		}
	}
	var followed atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Store(true) }))
	defer destination.Close()
	remote := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) })
	if _, err := remote.Manifest(t.Context(), "s1", "p1"); err == nil {
		t.Fatal("redirect accepted")
	}
	if followed.Load() {
		t.Fatal("redirect followed")
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS reached handler") }))
	defer tlsServer.Close()
	secure, err := NewRemote(tlsServer.URL, "token", false)
	if err != nil {
		t.Fatal(err)
	}
	defer secure.Close()
	if _, err = secure.Manifest(t.Context(), "s1", "p1"); !errors.Is(err, ErrTransport) {
		t.Fatal("untrusted TLS accepted", err)
	}
	if strings.Contains(err.Error(), tlsServer.URL) || strings.Contains(err.Error(), "token") {
		t.Fatal("raw network details leaked")
	}
}

func TestRemoteManifestValidationAndBasePath(t *testing.T) {
	rules, _ := syncproto.ParseRules("")
	valid := syncproto.ManifestResponse{Project: "p1", Revision: 1, Manifest: syncproto.Manifest{Version: 1, RulesHash: rules.Hash(), Entries: map[string]syncproto.Entry{}}}
	valid.Digest, _ = valid.Manifest.Digest()
	for _, kind := range []string{"valid", "digest", "project", "revision", "html", "error", "null", "invalid_tree", "large"} {
		t.Run(kind, func(t *testing.T) {
			remote := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/box/api/sessions/s1/sync/manifest" || r.URL.Query().Get("project") != "p1" {
					t.Error("base route", r.URL)
				}
				result := valid
				w.Header().Set("Content-Type", "application/json")
				switch kind {
				case "digest":
					result.Digest = strings.Repeat("0", 64)
				case "project":
					result.Project = "other"
				case "revision":
					result.Revision = 0
				case "html":
					w.Header().Set("Content-Type", "text/html")
					_, _ = io.WriteString(w, "<html>")
					return
				case "error":
					w.WriteHeader(503)
					return
				case "null":
					_, _ = io.WriteString(w, "null")
					return
				case "invalid_tree":
					result.Manifest.Entries = map[string]syncproto.Entry{"missing/file": remoteFileRequest().Expected}
				case "large":
					w.Header().Set("Content-Length", strconv.FormatInt(maxManifestJSON+1, 10))
					return
				}
				_ = json.NewEncoder(w).Encode(result)
			})
			remote.base.Path = "/box/"
			manifest, err := remote.Manifest(t.Context(), "s1", "p1")
			if kind == "valid" {
				if err != nil || manifest.Manifest.Entries == nil {
					t.Fatal(err)
				}
			} else if err == nil || manifest.Manifest.Entries != nil {
				t.Fatal("failure became valid empty tree", err, manifest)
			}
		})
	}
}

func TestRemoteLeaseNeverPutsSecretsInURLAndValidatesResponse(t *testing.T) {
	for _, kind := range []string{"valid", "owner", "generation", "path", "release_false"} {
		t.Run(kind, func(t *testing.T) {
			lease := syncproto.Lease{Workspace: "s1", Project: "p1", Device: "device", Path: ".", Token: "lease-secret", Generation: "generation", Expires: time.Now().Add(time.Minute)}
			remote := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.RawQuery != "" || r.Header.Get("X-Agentbox-Sync-Lease") != lease.Token {
					t.Error("lease boundary")
				}
				w.Header().Set("Content-Type", "application/json")
				result := lease
				switch kind {
				case "owner":
					result.Device = "other"
				case "generation":
					result.Generation = "other"
				case "path":
					result.Path = "other"
				case "release_false":
					_, _ = io.WriteString(w, `{"ok":false}`)
					return
				}
				_ = json.NewEncoder(w).Encode(result)
			})
			var err error
			if kind == "release_false" {
				err = remote.Release(t.Context(), lease)
			} else {
				_, err = remote.Renew(t.Context(), lease)
			}
			if kind == "valid" && err != nil || kind != "valid" && err == nil {
				t.Fatal(kind, err)
			}
		})
	}
}
