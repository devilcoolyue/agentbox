package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"agentbox/internal/chat"
	"agentbox/internal/usage"
)

// Keepalive points outlive the process. keepalive-points.json in the data
// directory holds each conversation's latest point, when a keepalive last
// read its cache and how often it did, so a restart or an upgrade plans again
// what it was keeping. Without it a chat waited for its next turn, and a
// terminal session kept past its hour never came back: the startup scan saw
// its last request over an hour old. The file is rewritten after each change,
// off the caller's path (keepaliveSaver), and once more as the server closes.
const keepaliveFile = "keepalive-points.json"

type keepaliveRecord struct {
	Workspace string            `json:"workspace"`
	Slot      string            `json:"slot"`
	Source    string            `json:"source"`
	Thread    string            `json:"thread,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Account   string            `json:"account"`
	Point     chat.CachePoint   `json:"point"`
	At        time.Time         `json:"at"`
	Warm      time.Time         `json:"warm"`
	Runs      int               `json:"runs,omitempty"`
	Last      usage.PromptCache `json:"last"`
}

type keepaliveRecords struct {
	Version int               `json:"version"`
	Points  []keepaliveRecord `json:"points"`
}

// changedLocked asks keepaliveSaver to write the points again; a no-op until
// restoreKeepalive set the file up.
func (k *keepaliveState) changedLocked() {
	select {
	case k.dirty <- struct{}{}:
	default:
	}
}

// recordsLocked lists the points whose cache may still be alive, with what
// their plans read so far.
func (k *keepaliveState) recordsLocked() []keepaliveRecord {
	now := time.Now()
	out := []keepaliveRecord{}
	for key, p := range k.points {
		plan := k.plans[key]
		if plan == nil && now.Sub(p.lastUse()) >= cacheLifetime {
			continue
		}
		r := keepaliveRecord{Workspace: key.workspace, Slot: key.slot, Source: p.source, Thread: p.thread, Cwd: p.cwd,
			Account: p.account, Point: p.point, At: p.at, Warm: p.warm}
		if plan != nil && plan.at.Equal(p.at) {
			r.Runs, r.Last = plan.runs, plan.last
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Workspace != out[j].Workspace {
			return out[i].Workspace < out[j].Workspace
		}
		return out[i].Slot < out[j].Slot
	})
	return out
}

// save writes the points to the file; saves are serialized so the file never
// goes back to an older snapshot.
func (k *keepaliveState) save() error {
	k.saveMu.Lock()
	defer k.saveMu.Unlock()
	k.mu.Lock()
	path, recs := k.path, k.recordsLocked()
	k.mu.Unlock()
	if path == "" {
		return nil
	}
	raw, err := json.Marshal(keepaliveRecords{Version: 1, Points: recs})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".keepalive-points-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// restoreKeepalive brings back the points a previous process saved at path,
// plans them, and saves there from now on. It runs before the terminal scan,
// which then finds the requests it reports again already known (note).
func (s *Server) restoreKeepalive(path string) {
	k := &s.keepalive
	k.mu.Lock()
	k.path, k.dirty = path, make(chan struct{}, 1)
	k.mu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("cache keepalive: restore: %v", err)
		}
		return
	}
	var saved keepaliveRecords
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Version != 1 {
		log.Printf("cache keepalive: restore: unreadable %s (%v)", path, err)
		return
	}
	restored := 0
	for _, r := range saved.Points {
		sess, ok := s.store.Get(r.Workspace)
		if !ok || r.Source != keepaliveChat && r.Source != keepaliveTerminal {
			continue
		}
		key := keepaliveKey{r.Workspace, r.Slot}
		pt := keepalivePoint{source: r.Source, thread: r.Thread, cwd: r.Cwd, account: r.Account, point: r.Point, at: r.At, warm: r.Warm}
		if time.Since(pt.lastUse()) >= cacheLifetime || !k.note(key, pt) {
			continue
		}
		s.planKeepalive(sess, key)
		k.mu.Lock()
		if p := k.plans[key]; p != nil && p.at.Equal(pt.at) {
			p.runs, p.last = r.Runs, r.Last
		}
		k.mu.Unlock()
		restored++
	}
	log.Printf("cache keepalive: restored %d of %d saved conversations", restored, len(saved.Points))
}

// keepaliveSaver writes the points after each change until the server stops;
// Close writes them a last time once every turn has ended.
func (s *Server) keepaliveSaver() {
	ctx := s.workContext()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.keepalive.dirty:
			if err := s.keepalive.save(); err != nil {
				log.Printf("cache keepalive: save: %v", err)
			}
		}
	}
}
