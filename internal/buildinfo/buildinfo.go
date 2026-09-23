// Package buildinfo exposes build metadata without initializing configuration,
// storage, Docker, networking or the browser UI.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

// Set by release builds through -ldflags -X. Plain go build remains useful.
var (
	Version  = "dev"
	Revision = "unknown"
	BuiltAt  = "unknown"
)

func Commit() string {
	revision := Revision
	if revision == "unknown" {
		if info, ok := debug.ReadBuildInfo(); ok {
			dirty := false
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					revision = setting.Value
				}
				if setting.Key == "vcs.modified" && setting.Value == "true" {
					dirty = true
				}
			}
			if dirty {
				revision += "+dirty"
			}
		}
	}
	return revision
}

func String(program string) string {
	return fmt.Sprintf("%s %s (commit %s, built %s)", program, Version, Commit(), BuiltAt)
}
