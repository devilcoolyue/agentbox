// Package usage normalizes events, prices usage and scans terminal transcripts.
// Settlement stays in Store's single transaction; terminal backfill never debits.
package usage

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/safefs"
	"agentbox/internal/store"
)

type Store interface {
	InsertUsage(...store.UsageEvent) error
	InsertUsageMessages(...store.UsageEvent) (int, error)
	UpsertTerminalUsage(...store.UsageEvent) error
	All() []store.Session
	Get(string) (store.Session, bool)
}
type Service struct {
	request    chan struct{}
	fullMu     sync.Mutex
	statusMu   sync.RWMutex
	status     SyncStatus
	scanErrors atomic.Uint64
	ctx        context.Context
	cfg        *config.Config
	store      Store
	termScan   *Scanner
}

func New(ctx context.Context, cfg *config.Config, st Store) *Service {
	return &Service{request: make(chan struct{}, 1), ctx: ctx, cfg: cfg, store: st, termScan: &Scanner{seen: map[string]termFileState{}}}
}
func (s *Service) workContext() context.Context { return s.ctx }
func (s *Service) homeDir(sess store.Session) string {
	return filepath.Join(s.cfg.DataDir, "users", sess.User, "sessions", sess.ID, "home")
}
func (s *Service) openDataDir(dir string) (*safefs.Root, error) {
	rel, err := filepath.Rel(s.cfg.DataDir, dir)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("invalid usage directory")
	}
	root, err := safefs.Open(s.cfg.DataDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Sub(rel)
}
func (s *Service) Scan()  { s.fullScan() }
func (s *Service) Loop()  { s.termUsageLoop() }
func (s *Service) Watch() { s.termWatchLoop() }
func waitInterval(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (s *Service) Price(ev store.UsageEvent) store.UsageEvent {
	return priceWith(s.cfg.PricingPlan(), ev)
}

func priceWith(plan config.PricingState, ev store.UsageEvent) store.UsageEvent {
	p, key, ok := config.LookupPrice(plan.Prices, ev.Agent, ev.Model)
	snapshot := &store.PriceSnapshot{Version: 1, Source: "unpriced", PricingRevision: plan.Revision}
	if origin, found := plan.Managed[key]; found {
		snapshot.CatalogVersion, snapshot.SourceURL, snapshot.VerifiedAt = origin.Version, origin.SourceURL, origin.VerifiedAt
	}
	if ok {
		snapshot.Key = key
		snapshot.Standard = store.TokenRates{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite}
		snapshot.LongContextOver = p.LongContextOver
		if p.Long != nil {
			snapshot.Long = &store.TokenRates{Input: p.Long.Input, Output: p.Long.Output, CacheRead: p.Long.CacheRead, CacheWrite: p.Long.CacheWrite}
		}
	}
	// Keep existing zero-cost fallback behavior, but persist the actual source
	// instead of labelling all Claude rows as provider-reported after the fact.
	if ev.CostMicroUSD != 0 && ev.Kind != store.UsageKindTerminal {
		snapshot.Source = "provider"
	} else if ok {
		snapshot.Source = "table"
		ev.CostMicroUSD = snapshot.Cost(ev)
	} else {
		ev.CostMicroUSD = 0
	}
	ev.Price = snapshot
	return ev
}
