package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"agentbox/internal/storagepolicy"
	"agentbox/internal/store"
	"agentbox/internal/toolstore"
)

// Reservations bound concurrent HTTP staging. Persistent writes are additionally
// charged to filesystem project quotas, including UID 1000 container writes.
func (s *Server) reserveStorageWrite(r *http.Request) (func(), bool) {
	if s.cfg.GetStoragePolicy().Mount == "" || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodDelete {
		return func() {}, true
	}
	bytes := int64(1 << 20)
	maxMB := s.cfg.GetMaxUploadMB()
	if maxMB < 1 || maxMB > 1<<30 {
		return nil, false
	}
	maxUpload := maxMB << 20
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") || r.ContentLength < 0 {
		bytes = maxUpload * 10 // request + archive + maximum extraction
		if r.ContentLength > 0 && r.ContentLength < maxUpload {
			bytes = max(1<<20, r.ContentLength*10)
		}
	} else if r.ContentLength > bytes {
		if r.ContentLength > 1<<40 {
			return nil, false
		}
		bytes = r.ContentLength * 2
	}
	reserve := s.cfg.GetResources().MinFreeBytes
	s.storageReserveMu.Lock()
	defer s.storageReserveMu.Unlock()
	for _, disk := range s.storageDisks() {
		if disk.Error || disk.Available < uint64(reserve+s.storageReserved+bytes) {
			return nil, false
		}
	}
	s.storageReserved += bytes
	return func() { s.storageReserveMu.Lock(); s.storageReserved -= bytes; s.storageReserveMu.Unlock() }, true
}

func (s *Server) storageOwner(user store.User) (string, error) {
	instance, err := s.clientIdentity()
	if err != nil {
		return "", err
	}
	return toolstore.OwnerKey(instance, user.Name, user.CreatedAt.UTC().Format(time.RFC3339Nano)), nil
}

func (s *Server) ensureUserStorage(ctx context.Context, user store.User) error {
	p := s.cfg.GetStoragePolicy()
	if p.Mount == "" {
		return nil
	}
	key, err := s.storageOwner(user)
	if err != nil {
		return err
	}
	s.storageUsersMu.Lock()
	defer s.storageUsersMu.Unlock()
	if s.storageUsers[key] {
		return nil
	}
	if _, err = storagepolicy.Ensure(ctx, p, s.cfg.DataDir, s.cfg.GetToolStorage().Root, user.Name, key); err != nil {
		log.Printf("storage: prepare user %s: %v", user.Name, err)
		return fmt.Errorf("prepare user storage: %w", err)
	}
	if s.storageUsers == nil {
		s.storageUsers = map[string]bool{}
	}
	s.storageUsers[key] = true
	return nil
}

func (s *Server) storageMaintenanceLoop() {
	for s.workContext().Err() == nil {
		p := s.cfg.GetStoragePolicy()
		tools := s.cfg.GetToolStorage().Root
		if tools != "" && (p.CacheMaxAgeDays > 0 || p.CacheTargetBytes > 0) {
			ctx, cancel := context.WithTimeout(s.workContext(), 30*time.Second)
			err := s.workspaces().MaintainStopped(ctx, func() error {
				for _, user := range s.store.ListUsers() {
					if err := ctx.Err(); err != nil {
						return err
					}
					owner, err := s.storageOwner(user)
					if err != nil {
						return err
					}
					target := p.CacheTargetBytes
					if target == 0 {
						limit := p.CacheBytes
						if own := p.CacheUsers[user.Name]; own > 0 {
							limit = own
						}
						target = limit * 3 / 4
					}
					result, err := storagepolicy.CleanCaches(ctx, tools, owner, target, time.Duration(p.CacheMaxAgeDays)*24*time.Hour)
					if err != nil {
						return err
					}
					if len(result) > 0 {
						log.Printf("storage: reclaimed rebuildable caches user=%s paths=%v", user.Name, result)
					}
				}
				return nil
			})
			cancel()
			if err != nil {
				log.Printf("storage: cache maintenance: %v", err)
			}
		}
		if !waitInterval(s.workContext(), time.Hour) {
			return
		}
	}
}
