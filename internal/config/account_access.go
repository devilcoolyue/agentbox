package config

import (
	"fmt"
	"maps"
	"slices"
)

// AccountAccess controls future credential delivery. Nil preserves legacy
// sharing. Administrators always retain access to the account pool.
type AccountAccess struct {
	Mode  string   `json:"mode"` // all | users | admin
	Users []string `json:"users,omitempty"`
}

func (a *AccountAccess) Validate() error {
	if a == nil {
		return nil
	}
	switch a.Mode {
	case "all", "admin":
		if len(a.Users) != 0 {
			return fmt.Errorf("access.users requires mode users")
		}
	case "users":
		if len(a.Users) > 1000 {
			return fmt.Errorf("access.users exceeds 1000 users")
		}
		seen := map[string]bool{}
		for _, user := range a.Users {
			if !accountIDRe.MatchString(user) || seen[user] {
				return fmt.Errorf("access.users contains invalid or duplicate username %q", user)
			}
			seen[user] = true
		}
	default:
		return fmt.Errorf("access.mode must be all, users or admin")
	}
	return nil
}

func (a Account) CanUse(user string, admin bool) bool {
	if admin {
		return true
	}
	if a.Access == nil || a.Access.Mode == "all" {
		return true
	}
	return a.Access.Mode == "users" && slices.Contains(a.Access.Users, user)
}

func cloneAccess(a *AccountAccess) *AccountAccess {
	if a == nil {
		return nil
	}
	return &AccountAccess{Mode: a.Mode, Users: slices.Clone(a.Users)}
}

func cloneAccount(a Account) Account {
	a.Access = cloneAccess(a.Access)
	a.Env = maps.Clone(a.Env)
	return a
}
