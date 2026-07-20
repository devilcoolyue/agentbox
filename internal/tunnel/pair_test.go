package tunnel

import "testing"

func TestPairCodeRoundTrip(t *testing.T) {
	code := EncodePairCode("https://box.example.com/", "s3cr3t-code")
	got, err := DecodePairCode(code)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The trailing slash is normalised away so the client can append paths.
	if got.Server != "https://box.example.com" {
		t.Errorf("server = %q", got.Server)
	}
	if got.Code != "s3cr3t-code" {
		t.Errorf("code = %q", got.Code)
	}
}

// Codes travel by copy-paste, which routinely adds wrapping and stray spaces.
func TestDecodePairCodeTolerantOfPasteNoise(t *testing.T) {
	code := EncodePairCode("https://box.example.com", "abc")
	noisy := "  " + code[:10] + "\n" + code[10:] + "\t\r\n"
	got, err := DecodePairCode(noisy)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Code != "abc" {
		t.Errorf("code = %q", got.Code)
	}
}

func TestDecodePairCodeRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"no prefix":   "aGVsbG8",
		"bad base64":  PairCodePrefix + "!!!not base64!!!",
		"not json":    PairCodePrefix + "aGVsbG8",
		"missing arg": PairCodePrefix + "eyJzIjoiaHR0cHM6Ly9hIn0", // {"s":"https://a"}, no code
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodePairCode(in); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}
