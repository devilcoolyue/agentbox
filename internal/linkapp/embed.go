package linkapp

import "embed"

// The control panel ships inside the binary, so the whole app stays one
// downloadable file — same trick agentbox's own web client uses.
//
//go:embed all:static
var staticFS embed.FS
