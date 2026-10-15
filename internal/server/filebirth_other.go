//go:build !linux

package server

import (
	"os"
	"time"
)

// fileBirth is not read outside Linux; callers fall back to the change time.
func fileBirth(*os.File) (time.Time, bool) { return time.Time{}, false }
