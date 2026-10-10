package server

import (
	"agentbox/internal/config"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStorageReservationUsesRequestSizeAndReleases(t *testing.T) {
	s := &Server{cfg: &config.Config{DataDir: t.TempDir(), MaxUploadMB: 1024 * 1024, StoragePolicy: config.StoragePolicy{Mount: "/fixture"}}}
	r := httptest.NewRequest("POST", "/upload", strings.NewReader("small file"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
	release, ok := s.reserveStorageWrite(r)
	if !ok {
		t.Fatal("small upload charged configured maximum")
	}
	if s.storageReserved != 1<<20 {
		t.Fatal(s.storageReserved)
	}
	release()
	if s.storageReserved != 0 {
		t.Fatal("reservation leaked")
	}
	s.cfg.Resources.MinFreeBytes = 1 << 62
	if _, ok := s.reserveStorageWrite(r); ok {
		t.Fatal("ignored free-space reserve")
	}
	r.Method = "DELETE"
	if release, ok := s.reserveStorageWrite(r); !ok {
		t.Fatal("blocked cleanup on full disk")
	} else {
		release()
	}
}
