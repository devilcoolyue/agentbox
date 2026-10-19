package store

// UsageWrite names one workspace whose usage rows a committed write added or
// changed. Listeners get identities only, never amounts: whoever is told
// re-reads through the normal, access-checked usage queries.
type UsageWrite struct {
	User      string
	SessionID string
}

// OnUsageWritten registers fn to run after every committed usage write (chat
// settlement, Claude message settlement, terminal backfill). It replaces any
// earlier listener; nil removes it. fn runs on the writer's goroutine after
// the commit, so it must return quickly and must not write usage itself.
func (s *Store) OnUsageWritten(fn func([]UsageWrite)) {
	if fn == nil {
		s.usageHook.Store(nil)
		return
	}
	s.usageHook.Store(&fn)
}

func (s *Store) usageWritten(evs []UsageEvent) {
	hook := s.usageHook.Load()
	if hook == nil || len(evs) == 0 {
		return
	}
	seen := make(map[UsageWrite]bool, 1)
	var touched []UsageWrite
	for _, e := range evs {
		w := UsageWrite{User: e.User, SessionID: e.SessionID}
		if !seen[w] {
			seen[w] = true
			touched = append(touched, w)
		}
	}
	(*hook)(touched)
}
