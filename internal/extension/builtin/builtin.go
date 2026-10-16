// Package builtin holds the official extensions compiled into agentbox. Each
// entry is an embedded manifest plus a Handler factory; the extension host
// validates the manifest like any other.
package builtin

import (
	_ "embed"

	"agentbox/pkg/extspi"
)

// Entry is one builtin extension.
type Entry struct {
	Manifest []byte
	New      func() extspi.Handler
}

//go:embed loop-guard.json
var loopGuardManifest []byte

// All returns the builtin extensions in display order.
func All() []Entry {
	return []Entry{
		{Manifest: loopGuardManifest, New: func() extspi.Handler { return &LoopGuard{} }},
	}
}
