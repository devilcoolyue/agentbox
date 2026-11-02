package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/storagepolicy"
	"agentbox/internal/store"
	"agentbox/internal/toolstore"
)

// storageExemptRoutes release resources or end work, so a full disk must never
// block them: users have to be able to stop what is filling it.
var storageExemptRoutes = map[string]bool{
	"POST /api/logout":                       true,
	"POST /api/sessions/{id}/stop":           true,
	"POST /api/sessions/{id}/keepalive/stop": true,
}

// storageRoutineRoutes barely touch the disk, or are how an operator recovers.
// They run down to the floor instead of stopping at the reserve; quotas still
// bound what a turn writes. Every other route keeps the full reserve.
var storageRoutineRoutes = map[string]bool{
	"POST /api/me/activity":                               true,
	"POST /api/sessions/{id}/start":                       true,
	"PUT /api/sessions/{id}/chat/requests/{request}":      true,
	"POST /api/sessions/{id}/chat/requests/{request}":     true,
	"POST /api/sessions/{id}/chat/questions/{question}":   true,
	"POST /api/sessions/{id}/chat/threads":                true,
	"POST /api/sessions/{id}/chat/threads/{tid}/activate": true,
	"PATCH /api/sessions/{id}/chat/threads/{tid}":         true,
	"POST /api/sessions/{id}/chat/reset":                  true,
	"POST /api/sessions/{id}/diagnostics":                 true,
	"POST /api/diagnostics":                               true,
	"POST /api/updates/check":                             true,
	"POST /api/me/password":                               true,
	"POST /api/users/{name}/password":                     true,
	"POST /api/users/{name}/credits":                      true,
	"PUT /api/users/{name}/quota":                         true,
	"PUT /api/users/{name}/storage":                       true,
	"PUT /api/settings":                                   true,
}

// readStorageDisks is replaced in tests that need a fixed amount of free space.
var readStorageDisks = (*Server).storageDisks

const smallWriteBytes = int64(1 << 20)

// Reservations bound concurrent HTTP staging. Persistent writes are additionally
// charged to filesystem project quotas, including UID 1000 container writes.
func (s *Server) reserveStorageWrite(r *http.Request) (func(), bool) {
	if s.cfg.GetStoragePolicy().Mount == "" || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodDelete || storageExemptRoutes[r.Pattern] {
		return func() {}, true
	}
	bytes := smallWriteBytes
	maxMB := s.cfg.GetMaxUploadMB()
	if maxMB < 1 || maxMB > 1<<30 {
		log.Printf("storage: refused write operation_id=%s route=%q: invalid upload limit", requestOperationID(r), r.Pattern)
		return nil, false
	}
	maxUpload := maxMB << 20
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") || r.ContentLength < 0 {
		bytes = maxUpload * 10 // request + archive + maximum extraction
		if r.ContentLength > 0 && r.ContentLength < maxUpload {
			bytes = max(smallWriteBytes, r.ContentLength*10)
		}
	} else if r.ContentLength > bytes {
		if r.ContentLength > 1<<40 {
			log.Printf("storage: refused write operation_id=%s route=%q: request too large", requestOperationID(r), r.Pattern)
			return nil, false
		}
		bytes = r.ContentLength * 2
	}
	limits := s.cfg.GetResources()
	reserve := limits.MinFreeBytes
	// A large body on a routine route still grows the disk, so it keeps the reserve.
	if storageRoutineRoutes[r.Pattern] && bytes == smallWriteBytes {
		reserve = limits.FloorBytes()
	}
	s.storageReserveMu.Lock()
	defer s.storageReserveMu.Unlock()
	for _, disk := range readStorageDisks(s) {
		if disk.Error || disk.Available < uint64(reserve+s.storageReserved+bytes) {
			log.Printf("storage: refused write operation_id=%s route=%q disk=%s disk_error=%t available=%d required=%d", requestOperationID(r), r.Pattern, strings.Join(disk.Roles, "/"), disk.Error, disk.Available, reserve+s.storageReserved+bytes)
			return nil, false
		}
	}
	s.storageReserved += bytes
	return func() { s.storageReserveMu.Lock(); s.storageReserved -= bytes; s.storageReserveMu.Unlock() }, true
}

func requestOperationID(r *http.Request) string {
	id, _ := r.Context().Value(operationContextKey{}).(string)
	return id
}

// refuseStorageWrite keeps HTTP 507 but answers as a catalogued problem, so the
// refusal is logged and the operation ID the user reports can be found.
func refuseStorageWrite(w http.ResponseWriter, r *http.Request) {
	p := operationProblem(r.Context(), "storage.write", "storage_full")
	w.Header().Set("X-Agentbox-Operation-ID", p.OperationID)
	writeJSON(w, http.StatusInsufficientStorage, p)
}

// lowStorageDisks lists the disks below the warning level. It is empty when no
// reserve is configured, since the service then refuses nothing on free space.
func (s *Server) lowStorageDisks() []storageDisk {
	limits := s.cfg.GetResources()
	if limits.MinFreeBytes <= 0 {
		return nil
	}
	var low []storageDisk
	for _, disk := range readStorageDisks(s) {
		if !disk.Error && disk.Available < uint64(limits.LowWaterBytes()) {
			low = append(low, disk)
		}
	}
	return low
}

// logStorageWater records each crossing once instead of once per pass, so the
// journal shows when a disk ran low and when it recovered.
func (s *Server) logStorageWater(wasLow map[string]bool) {
	low := map[string]uint64{}
	for _, disk := range s.lowStorageDisks() {
		low[strings.Join(disk.Roles, "/")] = disk.Available
	}
	for name, available := range low {
		if !wasLow[name] {
			wasLow[name] = true
			log.Printf("storage: disk running low disk=%s available=%d warn_below=%d", name, available, s.cfg.GetResources().LowWaterBytes())
		}
	}
	for name := range wasLow {
		if _, still := low[name]; !still {
			delete(wasLow, name)
			log.Printf("storage: disk recovered disk=%s", name)
		}
	}
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
	if _, err = storageEnsure(ctx, p, s.cfg.DataDir, s.cfg.GetToolStorage().Root, user.Name, key); err != nil {
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
	wasLow := map[string]bool{}
	for s.workContext().Err() == nil {
		s.logStorageWater(wasLow)
		p := s.cfg.GetStoragePolicy()
		tools := s.cfg.GetToolStorage().Root
		if tools != "" && (p.CacheMaxAgeDays > 0 || p.CacheTargetBytes > 0) {
			for _, user := range s.store.ListUsers() {
				if s.workContext().Err() != nil {
					return
				}
				// One user's oversized or unreadable cache must not cost the others their pass.
				if err := s.reclaimUserCaches(user, p, tools); err != nil {
					log.Printf("storage: cache maintenance user=%s: %v", user.Name, err)
				}
			}
		}
		if !waitInterval(s.workContext(), time.Hour) {
			return
		}
	}
}

func (s *Server) reclaimUserCaches(user store.User, p config.StoragePolicy, tools string) error {
	ctx, cancel := context.WithTimeout(s.workContext(), 30*time.Second)
	defer cancel()
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
	return s.workspaces().MaintainUser(ctx, user.Name, func() error {
		result, err := storagepolicy.CleanCaches(ctx, tools, owner, target, time.Duration(p.CacheMaxAgeDays)*24*time.Hour)
		if len(result) > 0 {
			log.Printf("storage: reclaimed rebuildable caches user=%s paths=%v", user.Name, result)
		}
		return err
	})
}
