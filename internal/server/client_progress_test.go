package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentbox/internal/syncclient"
)

func TestSyncProgressCountsBytesSeparatelyFromCommittedOperations(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "lost_response"}[fail], func(t *testing.T) {
			f := newEngineFixture(t, true, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if fail && strings.HasSuffix(r.URL.Path, "/sync/apply") {
						response := httptest.NewRecorder()
						next.ServeHTTP(response, r)
						w.Header().Set("X-Agentbox-Server-ID", response.Header().Get("X-Agentbox-Server-ID"))
						w.WriteHeader(503)
						return
					}
					next.ServeHTTP(w, r)
				})
			})
			content := strings.Repeat("abc\x00\r\n", 100_000)
			writeEngineFile(t, f.local, "upload.bin", content)
			var mu sync.Mutex
			var updates []syncclient.Progress
			ctx := syncclient.WithProgress(t.Context(), func(p syncclient.Progress) { mu.Lock(); updates = append(updates, p); mu.Unlock() })
			preview, err := f.engine.Preview(ctx, f.saved.ID, syncclient.Automatic)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := f.engine.Apply(ctx, f.saved.ID, preview, preview.Plan.Digest)
			if (err != nil) != fail {
				t.Fatal("unexpected result", err)
			}
			mu.Lock()
			defer mu.Unlock()
			var last syncclient.Progress
			scan := false
			inFlight := false
			for _, p := range updates {
				if p.Sequence <= last.Sequence || p.Bytes > p.TotalBytes || p.Completed > p.Total {
					t.Fatal("invalid progress", p)
				}
				if p.Stage == "local_scan" && p.ScannedBytes == int64(len(content)) {
					scan = true
				}
				if p.Stage == "applying" && p.Bytes > 0 && p.Completed == 0 {
					inFlight = true
				}
				last = p
			}
			if !scan || !inFlight || last.Total != 1 || last.Bytes != int64(len(content)) {
				t.Fatal("missing real scan/transfer progress", last)
			}
			if fail {
				if last.Completed != 0 || saved.Pending == nil {
					t.Fatal("failed upload claimed completion", last)
				}
			} else if last.Completed != 1 || last.Stage != "committing" || saved.Pending != nil {
				t.Fatal("successful batch did not verify", last)
			}
		})
	}
}
