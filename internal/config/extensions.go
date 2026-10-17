package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
)

// ErrExtensionRevision rejects a write based on a stale extension setting.
var ErrExtensionRevision = errors.New("扩展设置已变化，请刷新后重试")

var extensionIDRe = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)

// ExtensionsConfig holds the administrator's per-extension settings. They are
// validated structurally here; the extension host validates them against each
// manifest before they are saved.
type ExtensionsConfig struct {
	Items map[string]ExtensionSetting `json:"items,omitempty"`
	// LegacyGateway is the pre-2026-10-17 home of model_gateway. Load moves it
	// there and clears it; it is never written back.
	LegacyGateway *ModelGatewayConfig `json:"gateway,omitempty"`
}

type ExtensionSetting struct {
	Enabled       bool              `json:"enabled"`
	Version       string            `json:"version,omitempty"`
	Order         int               `json:"order"`
	Config        json.RawMessage   `json:"config,omitempty"`
	Permissions   []string          `json:"permissions"`
	Bindings      ExtensionBindings `json:"bindings"`
	FailurePolicy string            `json:"failure_policy,omitempty"`
	Revision      int64             `json:"revision"`
}

// ExtensionBindings selects where an extension applies: nowhere, everywhere
// its hooks can fire (including accounts and users added later), or the
// selected accounts and users. Which selectors an extension accepts comes
// from its manifest; a hook fires when either list matches.
type ExtensionBindings struct {
	Mode     string   `json:"mode"` // none | all | selected
	Accounts []string `json:"accounts,omitempty"`
	Users    []string `json:"users,omitempty"`
}

// Binding modes. BindingModeAccounts is the pre-2026-10-17 name of
// selected, still accepted on input and rewritten.
const (
	BindingModeNone     = "none"
	BindingModeAll      = "all"
	BindingModeSelected = "selected"
	BindingModeAccounts = "accounts"
)

// BindingTarget is what a hook fired for: the account and user involved, as
// far as they are known.
type BindingTarget struct {
	Account string
	User    string
}

// Normalized rewrites the legacy accounts mode to selected.
func (b ExtensionBindings) Normalized() ExtensionBindings {
	if b.Mode == BindingModeAccounts {
		b.Mode = BindingModeSelected
	}
	return b
}

func (s ExtensionSetting) Clone() ExtensionSetting {
	s.Config = append(json.RawMessage(nil), s.Config...)
	s.Permissions = append([]string(nil), s.Permissions...)
	s.Bindings.Accounts = append([]string(nil), s.Bindings.Accounts...)
	s.Bindings.Users = append([]string(nil), s.Bindings.Users...)
	return s
}

// Binds reports whether the setting applies to the target.
func (s ExtensionSetting) Binds(t BindingTarget) bool {
	switch s.Bindings.Normalized().Mode {
	case BindingModeAll:
		return true
	case BindingModeSelected:
		return (t.Account != "" && slices.Contains(s.Bindings.Accounts, t.Account)) ||
			(t.User != "" && slices.Contains(s.Bindings.Users, t.User))
	}
	return false
}

// normalizeLoaded undoes the indentation json.MarshalIndent adds to raw
// extension configs on save, keeping the in-memory form canonical, and
// rewrites legacy binding modes.
func (e *ExtensionsConfig) normalizeLoaded() {
	for id, s := range e.Items {
		var buf bytes.Buffer
		if len(s.Config) != 0 && json.Compact(&buf, s.Config) == nil {
			s.Config = append(json.RawMessage(nil), buf.Bytes()...)
		}
		s.Bindings = s.Bindings.Normalized()
		e.Items[id] = s
	}
}

func (e ExtensionsConfig) Clone() ExtensionsConfig {
	out := ExtensionsConfig{}
	if e.Items != nil {
		out.Items = make(map[string]ExtensionSetting, len(e.Items))
		for id, s := range e.Items {
			out.Items[id] = s.Clone()
		}
	}
	return out
}

func (c *Config) validateExtensions() error {
	e := c.Extensions
	accounts := map[string]bool{}
	for _, a := range c.Accounts {
		accounts[a.ID] = true
	}
	for id, s := range e.Items {
		if len(id) > 64 || !extensionIDRe.MatchString(id) {
			return fmt.Errorf("extensions.items: invalid extension id %q", id)
		}
		if s.Order < 0 || s.Order > 1000 {
			return fmt.Errorf("extensions.items.%s.order must be 0..1000", id)
		}
		if s.Revision < 0 || s.Revision == math.MaxInt64 {
			return fmt.Errorf("extensions.items.%s.revision invalid", id)
		}
		if len(s.Config) > 64<<10 || (len(s.Config) != 0 && !json.Valid(s.Config)) {
			return fmt.Errorf("extensions.items.%s.config must be JSON of at most 64 KiB", id)
		}
		if len(s.Permissions) > 16 {
			return fmt.Errorf("extensions.items.%s.permissions too long", id)
		}
		switch s.FailurePolicy {
		case "", "fail_open", "fail_closed":
		default:
			return fmt.Errorf("extensions.items.%s.failure_policy must be fail_open or fail_closed", id)
		}
		switch s.Bindings.Mode {
		case BindingModeNone, BindingModeAll:
			if len(s.Bindings.Accounts) != 0 || len(s.Bindings.Users) != 0 {
				return fmt.Errorf("extensions.items.%s.bindings: accounts and users only apply to mode selected", id)
			}
		case BindingModeSelected:
			seen := map[string]bool{}
			for _, acct := range s.Bindings.Accounts {
				if !accounts[acct] || seen[acct] {
					return fmt.Errorf("extensions.items.%s.bindings: unknown or duplicate account %q", id, acct)
				}
				seen[acct] = true
			}
			// Users live in the database; the server checks they exist.
			seen = map[string]bool{}
			for _, user := range s.Bindings.Users {
				if user == "" || len(user) > 64 || seen[user] {
					return fmt.Errorf("extensions.items.%s.bindings: invalid or duplicate user %q", id, user)
				}
				seen[user] = true
			}
		default:
			return fmt.Errorf("extensions.items.%s.bindings.mode must be none, selected or all", id)
		}
	}
	return nil
}

// GetExtensions returns a deep copy of the extension settings.
func (c *Config) GetExtensions() ExtensionsConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Extensions.Clone()
}

// SetExtension replaces one extension's setting if expectedRevision matches
// the stored one (0 for a setting that does not exist yet). The saved setting
// gets the next revision.
func (c *Config) SetExtension(id string, s ExtensionSetting, expectedRevision int64) (ExtensionSetting, error) {
	var out ExtensionSetting
	err := c.mutate(func(w *Config) error {
		current := w.Extensions.Items[id]
		if current.Revision != expectedRevision {
			return ErrExtensionRevision
		}
		next := s.Clone()
		next.Bindings = next.Bindings.Normalized()
		next.Revision = current.Revision + 1
		items := make(map[string]ExtensionSetting, len(w.Extensions.Items)+1)
		for k, v := range w.Extensions.Items {
			items[k] = v
		}
		items[id] = next
		w.Extensions.Items = items
		out = next.Clone()
		return nil
	})
	return out, err
}

// pruneExtensionAccount drops a deleted account from every binding and bumps
// the affected revisions, so a stale browser cannot write it back.
func (w *Config) pruneExtensionAccount(id string) {
	w.pruneExtensionBindings(func(b *ExtensionBindings) *[]string { return &b.Accounts }, id)
}

// PruneExtensionUser drops a deleted user from every binding, like
// pruneExtensionAccount. Users live in the database, so the server calls it
// after deleting one.
func (c *Config) PruneExtensionUser(name string) error {
	return c.mutate(func(w *Config) error {
		w.pruneExtensionBindings(func(b *ExtensionBindings) *[]string { return &b.Users }, name)
		return nil
	})
}

func (w *Config) pruneExtensionBindings(list func(*ExtensionBindings) *[]string, name string) {
	var items map[string]ExtensionSetting
	for key, s := range w.Extensions.Items {
		if !slices.Contains(*list(&s.Bindings), name) {
			continue
		}
		if items == nil {
			items = make(map[string]ExtensionSetting, len(w.Extensions.Items))
			for k, v := range w.Extensions.Items {
				items[k] = v
			}
		}
		next := s.Clone()
		l := list(&next.Bindings)
		*l = slices.DeleteFunc(*l, func(v string) bool { return v == name })
		next.Revision++
		items[key] = next
	}
	if items != nil {
		w.Extensions.Items = items
	}
}
