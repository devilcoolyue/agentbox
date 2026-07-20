package tunnel

import "testing"

func TestParseMapSpec(t *testing.T) {
	good := []struct {
		in   string
		port int
		tgt  string
	}{
		{"3306=10.0.1.5:3306", 3306, "10.0.1.5:3306"},
		{" 15432=db.corp.local:5432 ", 15432, "db.corp.local:5432"},
		{"8080=[fd00::1]:80", 8080, "[fd00::1]:80"},
	}
	for _, c := range good {
		m, err := ParseMapSpec(c.in)
		if err != nil || m.Port != c.port || m.Target != c.tgt {
			t.Errorf("ParseMapSpec(%q) = %+v, %v; want port=%d target=%q", c.in, m, err, c.port, c.tgt)
		}
	}

	bad := []string{"", "3306", "=10.0.1.5:3306", "x=10.0.1.5:3306", "0=10.0.1.5:3306",
		"70000=10.0.1.5:3306", "3306=10.0.1.5", "3306=:3306"}
	for _, c := range bad {
		if _, err := ParseMapSpec(c); err == nil {
			t.Errorf("ParseMapSpec(%q) succeeded, want error", c)
		}
	}
}

func TestMapSpecsRoundTrip(t *testing.T) {
	specs := []MapSpec{{3306, "10.0.1.5:3306"}, {16379, "redis.corp.local:6379"}}
	enc := EncodeMapSpecs(specs)
	if enc != "3306=10.0.1.5:3306,16379=redis.corp.local:6379" {
		t.Fatalf("encoded %q", enc)
	}
	dec, err := ParseMapSpecs(enc)
	if err != nil || len(dec) != 2 || dec[0] != specs[0] || dec[1] != specs[1] {
		t.Fatalf("round trip: %+v, %v", dec, err)
	}
	if got, err := ParseMapSpecs(""); err != nil || len(got) != 0 {
		t.Fatalf("empty header: %+v, %v", got, err)
	}
	if _, err := ParseMapSpecs("3306=oops"); err == nil {
		t.Fatal("bad entry accepted")
	}
}
