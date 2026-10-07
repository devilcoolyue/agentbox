package password

import "testing"

func TestPersistedPasswordCompatibility(t *testing.T) {
	// Independently generated with Python hashlib.pbkdf2_hmac, using the
	// pre-recovery login format and iteration count. Entirely synthetic.
	const legacy = "pbkdf2-sha256$210000$00112233445566778899aabbccddeeff$c7e83b20fcc7a270e311cadd062710968738a94fedfc6e30ae2bfc5892f9d9f4"
	if !Verify(legacy, "synthetic-legacy-password") || Verify(legacy, "wrong-password") {
		t.Fatal("persisted login format is no longer compatible")
	}
	first, second := Hash("synthetic-new-password"), Hash("synthetic-new-password")
	if first == second || !Verify(first, "synthetic-new-password") || !Verify(second, "synthetic-new-password") {
		t.Fatal("new passwords must use distinct salts and verify")
	}
	for _, bad := range []string{"", "plain", "pbkdf2-sha256$bad$00$00", "pbkdf2-sha256$0$00$00", "pbkdf2-sha256$1$not-hex$00", "pbkdf2-sha256$1$00$"} {
		if Verify(bad, "synthetic-new-password") {
			t.Fatal("accepted malformed hash")
		}
	}
}
