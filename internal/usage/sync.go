package usage

import "time"

// SyncStatus describes the last full pass, including failed reads/writes.
// Watcher updates can be newer; this timestamp is not a provider delivery guarantee.
type SyncStatus struct {
	LastScanAt    int64  `json:"last_scan_at"`
	LastSuccessAt int64  `json:"last_success_at"`
	Scanning      bool   `json:"scanning"`
	Errors        uint64 `json:"errors"`
}

func (s *Service) RequestScan() {
	select {
	case s.request <- struct{}{}:
	default:
	}
}
func (s *Service) SyncStatus() SyncStatus {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.status
}
func (s *Service) fullScan() {
	s.fullMu.Lock()
	defer s.fullMu.Unlock()
	s.statusMu.Lock()
	s.status.Scanning = true
	s.statusMu.Unlock()
	before := s.scanErrors.Load()
	s.scanTerminalUsage(s.termScan)
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.status.Scanning = false
	if s.ctx.Err() != nil {
		return
	}
	s.status.LastScanAt = time.Now().UnixMilli()
	s.status.Errors = s.scanErrors.Load() - before
	if s.status.Errors == 0 {
		s.status.LastSuccessAt = s.status.LastScanAt
	}
}
