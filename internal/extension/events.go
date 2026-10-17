package extension

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"agentbox/internal/config"
	"agentbox/pkg/extspi"
)

// The event hooks (workspace.lifecycle, chat.turn.ended): notifications after
// the fact. Nothing in agentbox waits for an extension to handle one, so they
// have no failure policy; an event the instance cannot take (not running,
// queue full) is dropped and counted in its log.

// eventNoticeSize is what one queued event counts against the queue budget,
// bounding a stalled extension's backlog to a few thousand events.
const eventNoticeSize = 1 << 10

// implementsHook reports whether handler implements the interface a hook's
// kind needs.
func implementsHook(handler extspi.Handler, spec extspi.HookSpec) bool {
	switch spec.Kind {
	case extspi.KindInspect:
		_, ok := handler.(extspi.ModelHandler)
		return ok
	case extspi.KindEvent:
		_, ok := handler.(extspi.EventHandler)
		return ok
	}
	return false
}

// Emit delivers an event hook to every running extension subscribed to it
// whose bindings include target. It never blocks on the extensions.
func (h *Host) Emit(hook string, target config.BindingTarget, p extspi.EventParams) {
	if h == nil {
		return
	}
	var id [12]byte
	_, _ = rand.Read(id[:])
	p.Hook, p.ID, p.Time = hook, "evt_"+hex.EncodeToString(id[:]), time.Now().UTC()
	h.mu.Lock()
	var list []*instance
	for _, inst := range h.orderedLocked() {
		if inst.uses([]string{hook}) && inst.setting.Binds(target) {
			list = append(list, inst)
		}
	}
	h.mu.Unlock()
	for _, in := range list {
		if _, _, hooks, ok := in.running(); !ok || !hooks[hook] {
			continue
		}
		if in.enqueue(&eventNotice{p: p}) == overBudget {
			in.log("warn", "event queue full; dropped a "+hook+" event")
		}
	}
}

type eventNotice struct{ p extspi.EventParams }

func (n *eventNotice) size() int                { return eventNoticeSize }
func (n *eventNotice) absorb(notice) bool       { return false }
func (n *eventNotice) deliver(h extspi.Handler) { h.(extspi.EventHandler).Event(n.p) }
