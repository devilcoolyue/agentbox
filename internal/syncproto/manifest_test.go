package syncproto

import "testing"

func TestManifestStructureAndDeterministicDigest(t *testing.T) {
	rules, _ := ParseRules("")
	file := Entry{Kind: "file", Hash: HashBytes([]byte("content")), Size: 7}
	m := Manifest{Version: 1, RulesHash: rules.Hash(), Entries: map[string]Entry{"src": {Kind: "directory"}, "src/a": file}}
	one, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	n := Manifest{Version: 1, RulesHash: rules.Hash(), Entries: map[string]Entry{"src/a": file, "src": {Kind: "directory"}}}
	two, err := n.Digest()
	if err != nil || one != two {
		t.Fatalf("unstable digest %s %s %v", one, two, err)
	}
	delete(n.Entries, "src")
	if n.Validate() == nil {
		t.Fatal("missing parent accepted")
	}
	n.Entries["src"] = file
	if n.Validate() == nil {
		t.Fatal("file as parent accepted")
	}
	for _, name := range []string{".", "../x", "/a", "a\\b", "x/../y", "a//b", "a\x00b"} {
		if ValidPath(name) {
			t.Errorf("unsafe path %q", name)
		}
	}
}

func TestIgnoreSubsetAndHardExclusions(t *testing.T) {
	rules, err := ParseRules("# comment\n*.log\n/cache/\nsrc/**/generated/*.ts\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git/config", "nested/.git/HEAD", "node_modules/x", "cache/sub/file", "a/test.log", "src/generated/a.ts", "src/a/b/generated/a.ts", ".agentbox-sync/history/x", "a/.agentbox-sync-tmp-test"} {
		if !rules.Ignored(name) {
			t.Errorf("not ignored: %s", name)
		}
	}
	for _, name := range []string{".agentboxignore", "src/a.ts", "src/a/generated/a.js", "src/generated-file.ts"} {
		if rules.Ignored(name) {
			t.Errorf("unexpected ignore: %s", name)
		}
	}
	for _, text := range []string{"!node_modules", "foo/**bar", "../x", "a\\b", "[invalid", "a//b"} {
		if _, err := ParseRules(text); err == nil {
			t.Errorf("silently accepted unsupported rule %q", text)
		}
	}
	equivalent, _ := ParseRules("*.log\ncache\nsrc/**/generated/*.ts")
	if rules.Hash() != equivalent.Hash() {
		t.Fatal("normalized rules not stable")
	}
}
