package syncproto

import (
	"strings"
	"testing"
)

func TestMutationIntentDigestFencesChangesButAllowsNewLease(t *testing.T) {
	rules, _ := ParseRules("")
	entry := &Entry{Kind: "file", Hash: HashBytes([]byte("x")), Size: 1}
	m := Mutation{Version: 1, ID: strings.Repeat("a", 32), Project: "project", Revision: 1, RulesHash: rules.Hash(), Device: "device", Generation: "first", Path: "file", Kind: "replace", After: entry}
	digest, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	m.Generation = "next"
	next, err := m.Digest()
	if err != nil || next != digest {
		t.Fatal("lease renewal changed intent", err)
	}
	m.Path = "other"
	next, err = m.Digest()
	if err != nil || next == digest {
		t.Fatal("path not covered", err)
	}
	for _, kind := range []string{"id", "path", "after", "kind", "version"} {
		copy := m
		switch kind {
		case "id":
			copy.ID = "../outside"
		case "path":
			copy.Path = "../outside"
		case "after":
			copy.After = nil
		case "kind":
			copy.Kind = "rmtree"
		case "version":
			copy.Version = 2
		}
		if copy.Validate() == nil {
			t.Fatal("invalid accepted", kind)
		}
	}
}
