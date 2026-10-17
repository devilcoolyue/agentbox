package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"agentbox/pkg/extspi"
	"agentbox/pkg/loopguard"
	"agentbox/pkg/modelgw"
)

// maxTracked bounds per-stream state if a host ever fails to close streams.
const maxTracked = 4096

// LoopGuard is the agentbox.loop-guard extension: the standalone
// abox-loop-guard detector and cooldown behind the SPI.
type LoopGuard struct {
	extspi.BaseHandler
	host     extspi.ModelHost
	window   int
	distinct int
	ttl      time.Duration
	cooldown *loopguard.Cooldown

	mu      sync.Mutex
	pending map[string][32]byte // fingerprint by stream, from model/request
	streams map[string]*lgStream
	trips   float64
	rejects float64
}

type lgStream struct {
	key     [32]byte
	monitor *loopguard.Monitor
	tripped bool
}

type lgConfig struct {
	Window          int `json:"window"`
	DistinctLimit   int `json:"distinct_limit"`
	CooldownSeconds int `json:"cooldown_seconds"`
}

func (g *LoopGuard) Initialize(_ context.Context, p extspi.InitializeParams, host extspi.Host) (extspi.InitializeResult, error) {
	cfg := lgConfig{Window: loopguard.DefaultWindow, DistinctLimit: loopguard.DefaultDistinctLimit, CooldownSeconds: 30}
	if len(p.Config) != 0 {
		if err := json.Unmarshal(p.Config, &cfg); err != nil {
			return extspi.InitializeResult{}, &extspi.Error{Code: extspi.CodeInvalidParams, Message: "invalid config"}
		}
	}
	mh, ok := host.(extspi.ModelHost)
	if !ok {
		return extspi.InitializeResult{}, &extspi.Error{Code: extspi.CodeInternalError, Message: "host cannot interrupt streams"}
	}
	g.host, g.window, g.distinct = mh, cfg.Window, cfg.DistinctLimit
	g.ttl = time.Duration(cfg.CooldownSeconds) * time.Second
	g.cooldown = loopguard.NewCooldown(g.ttl)
	g.pending, g.streams = map[string][32]byte{}, map[string]*lgStream{}
	var hooks []string
	for _, perm := range p.Permissions {
		switch perm {
		case extspi.PermRequestFingerprint:
			hooks = append(hooks, extspi.HookModelRequest)
		case extspi.PermStreamText:
			hooks = append(hooks, extspi.HookModelStream)
		}
	}
	return extspi.InitializeResult{SPIVersion: extspi.Version, Hooks: hooks}, nil
}

// fingerprintKey turns the host's salted hex fingerprint into a cooldown key.
func fingerprintKey(fp string) [32]byte {
	var key [32]byte
	if b, err := hex.DecodeString(fp); err == nil && len(b) == len(key) {
		copy(key[:], b)
		return key
	}
	return sha256.Sum256([]byte(fp))
}

func (g *LoopGuard) ModelRequest(_ context.Context, p extspi.ModelRequestParams) (extspi.RequestVerdict, error) {
	key := fingerprintKey(p.Fingerprint)
	if p.Fingerprint != "" && g.cooldown.Blocked(key) {
		g.mu.Lock()
		g.rejects++
		g.mu.Unlock()
		g.report()
		return extspi.RequestVerdict{Action: extspi.ActionReject, Code: loopguard.ErrorCode, Message: loopguard.CooldownMessage(g.ttl)}, nil
	}
	g.mu.Lock()
	if len(g.pending) >= maxTracked {
		clear(g.pending)
	}
	if p.Fingerprint != "" {
		g.pending[p.Stream] = key
	}
	g.mu.Unlock()
	return extspi.RequestVerdict{Action: extspi.ActionAllow}, nil
}

func (g *LoopGuard) StreamOpen(p extspi.StreamOpenParams) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.streams) >= maxTracked {
		clear(g.streams)
	}
	g.streams[p.Stream] = &lgStream{key: g.pending[p.Stream], monitor: loopguard.NewMonitor(g.window, g.distinct)}
	delete(g.pending, p.Stream)
}

func (g *LoopGuard) StreamDelta(p extspi.StreamDeltaParams) {
	g.mu.Lock()
	s := g.streams[p.Stream]
	if s == nil || s.tripped {
		g.mu.Unlock()
		return
	}
	deltas := make([]modelgw.Delta, len(p.Deltas))
	for i, d := range p.Deltas {
		deltas[i] = modelgw.Delta{Part: d.Part, Channel: d.Channel, Text: d.Text}
	}
	hit := s.monitor.Push(deltas, p.EndedParts)
	if hit != nil {
		s.tripped = true
		g.trips++
	}
	key := s.key
	g.mu.Unlock()
	if hit == nil {
		return
	}
	var zero [32]byte
	if key != zero {
		g.cooldown.Block(key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = g.host.Abort(ctx, extspi.AbortParams{Stream: p.Stream, Code: loopguard.ErrorCode, Message: loopguard.TripMessage(*hit),
		Detail: map[string]float64{"window": float64(hit.Window), "distinct": float64(hit.Distinct)}})
	g.host.Log(extspi.LogParams{Level: "info", Message: "interrupted a repeating " + hit.Channel + " stream"})
	g.report()
}

func (g *LoopGuard) StreamClose(p extspi.StreamCloseParams) {
	g.mu.Lock()
	delete(g.pending, p.Stream)
	delete(g.streams, p.Stream)
	g.mu.Unlock()
}

func (g *LoopGuard) report() {
	g.mu.Lock()
	metrics := map[string]float64{"trips": g.trips, "cooldown_rejects": g.rejects, "cooldown_entries": float64(g.cooldown.Len())}
	g.mu.Unlock()
	g.host.ReportStatus(extspi.StatusReport{State: "ok", Metrics: metrics})
}
