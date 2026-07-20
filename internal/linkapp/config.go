// Package linkapp turns abox-link from a flags-and-a-terminal tool into a
// piece of software: it keeps the link's settings in a file, supervises the
// tunnel so it can be started and stopped on demand, and serves a small local
// control panel on the loopback interface for configuring all of it.
//
// The panel is the default experience (run the binary with no flags); the
// original one-shot CLI mode stays available for headless and scripted use.
package linkapp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentbox/internal/tunnel"
)

// Config is everything the panel persists between runs. The password is
// deliberately absent: pairing exchanges it once for a token, and only the
// token is stored.
type Config struct {
	Server      string   `json:"server"`       // agentbox base URL
	User        string   `json:"user"`         // agentbox username
	Token       string   `json:"token"`        // session token from pairing/login
	Insecure    bool     `json:"insecure"`     // skip TLS verification
	Allow       []string `json:"allow"`        // whitelist rules (CIDR / host / host:port)
	Maps        []string `json:"maps"`         // port maps, "PORT=HOST:PORT"
	AutoConnect bool     `json:"auto_connect"` // dial the tunnel as soon as the app starts
}

// Paired reports whether the config carries usable server credentials.
func (c Config) Paired() bool {
	return c.Server != "" && c.User != "" && c.Token != ""
}

// Validate checks the parts the user edits in the panel, returning a message
// meant to be shown verbatim in the UI. It also reports the parsed forms so
// callers do not parse twice.
func (c Config) Validate() (tunnel.Whitelist, []tunnel.MapSpec, error) {
	var maps []tunnel.MapSpec
	allow := append([]string(nil), c.Allow...)
	for _, m := range c.Maps {
		spec, err := tunnel.ParseMapSpec(m)
		if err != nil {
			return nil, nil, err
		}
		maps = append(maps, spec)
		allow = append(allow, spec.Target) // a mapped target is implicitly allowed
	}
	if len(allow) == 0 {
		return nil, nil, fmt.Errorf("至少要有一条放行规则或端口映射（默认拒绝一切）")
	}
	wl, err := tunnel.ParseWhitelist(allow)
	if err != nil {
		return nil, nil, err
	}
	return wl, maps, nil
}

// ConfigDir is where the panel keeps its state, overridable for tests and for
// service installs that run as another user.
func ConfigDir() string {
	if d := os.Getenv("ABOX_LINK_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".abox-link"
	}
	return filepath.Join(home, ".abox-link")
}

func configPath() string { return filepath.Join(ConfigDir(), "config.json") }

// LoadConfig reads the stored config; a missing file yields a zero Config and
// no error, which the panel renders as the first-run pairing screen.
func LoadConfig() (Config, error) {
	var c Config
	raw, err := os.ReadFile(configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("配置文件损坏: %w", err)
	}
	c.Server = strings.TrimRight(strings.TrimSpace(c.Server), "/")
	return c, nil
}

// SaveConfig writes the config atomically with owner-only permissions — it
// holds a session token.
func SaveConfig(c Config) error {
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := configPath() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath())
}
