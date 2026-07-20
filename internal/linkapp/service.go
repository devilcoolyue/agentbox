package linkapp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Autostart registers the app with the OS session manager so the tunnel comes
// back by itself after a reboot — the difference between "a program I remember
// to run" and "a service that is just on".
//
// Each platform gets its own file (service_linux.go, service_darwin.go,
// service_windows.go) because the mechanisms share nothing but their shape:
// a systemd user unit, a LaunchAgent, and a logon scheduled task.

// ServiceName is the identifier used for the unit / agent / task.
const ServiceName = "abox-link"

// ServiceStatus describes autostart support on this machine.
type ServiceStatus struct {
	Supported bool   `json:"supported"` // the platform mechanism exists here
	Installed bool   `json:"installed"` // autostart is currently registered
	Detail    string `json:"detail"`    // what the user should know, shown in the panel
}

// exePath resolves the running binary to an absolute, symlink-free path — the
// service definition outlives this process and cannot rely on $PATH or cwd.
func exePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Abs(p)
}

// run executes a command and folds its output into any error, since the
// message goes straight to the panel.
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			return fmt.Errorf("%s: %w", name, err)
		}
		return fmt.Errorf("%s: %w: %s", name, err, detail)
	}
	return nil
}

// writeFileMode writes a service definition, creating parent directories.
func writeServiceFile(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), mode)
}

// configHomeOverride returns the ABOX_LINK_HOME value the service must inherit,
// or "" when the default location applies. Without this, installing autostart
// from a panel running on a custom config dir would silently produce a service
// that reads a different, probably empty, config.
func configHomeOverride() string { return os.Getenv("ABOX_LINK_HOME") }
