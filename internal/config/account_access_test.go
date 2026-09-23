package config

import (
	"os"
	"testing"
)

func TestAccountAccessCompatibilityAndPolicy(t *testing.T) {
	for _, tt := range []struct {
		name       string
		access     *AccountAccess
		alice, bob bool
	}{
		{"legacy", nil, true, true},
		{"all", &AccountAccess{Mode: "all"}, true, true},
		{"selected", &AccountAccess{Mode: "users", Users: []string{"alice"}}, true, false},
		{"empty", &AccountAccess{Mode: "users"}, false, false},
		{"admin", &AccountAccess{Mode: "admin"}, false, false},
		{"unknown fails closed", &AccountAccess{Mode: "future"}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := Account{Access: tt.access}
			if a.CanUse("alice", false) != tt.alice || a.CanUse("bob", false) != tt.bob || !a.CanUse("root", true) {
				t.Fatal("incorrect access policy")
			}
		})
	}
}

func TestAccountAccessPersistenceAndIsolation(t *testing.T) {
	c := writeConfig(t, minimalConfig)
	a := Account{ID: "team", Type: AgentClaude, Access: &AccountAccess{Mode: "users", Users: []string{"alice"}}}
	if err := c.AddAccount(a); err != nil {
		t.Fatal(err)
	}
	a.Access.Users[0] = "bob"
	got, _ := c.Account("team")
	got.Access.Users[0] = "bob"
	list := c.AccountList()
	list[0].Access.Users[0] = "bob"
	label := "renamed"
	if _, err := c.UpdateAccount("team", AccountPatch{Label: &label}); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplySettings(SettingsPatch{}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	got, _ = reloaded.Account("team")
	if got.Access.Users[0] != "alice" {
		t.Fatalf("policy changed through snapshot or settings save: %+v", got.Access)
	}
	before, _ := os.ReadFile(c.Path())
	for _, bad := range []*AccountAccess{
		{Mode: ""}, {Mode: "typo"}, {Mode: "all", Users: []string{"alice"}},
		{Mode: "users", Users: []string{"../alice"}}, {Mode: "users", Users: []string{"alice", "alice"}},
	} {
		if _, err := c.UpdateAccount("team", AccountPatch{Access: bad}); err == nil {
			t.Fatalf("accepted invalid policy %+v", bad)
		}
	}
	after, _ := os.ReadFile(c.Path())
	if string(before) != string(after) {
		t.Fatal("failed update modified disk")
	}
	got, _ = c.Account("team")
	if !got.CanUse("alice", false) || got.CanUse("bob", false) {
		t.Fatal("failed update modified memory")
	}
	policy := &AccountAccess{Mode: "users", Users: []string{"bob"}}
	updated, err := c.UpdateAccount("team", AccountPatch{Access: policy})
	if err != nil {
		t.Fatal(err)
	}
	policy.Users[0], updated.Access.Users[0] = "alice", "alice"
	got, _ = c.Account("team")
	if !got.CanUse("bob", false) || got.CanUse("alice", false) {
		t.Fatal("update aliased input/output")
	}
}
