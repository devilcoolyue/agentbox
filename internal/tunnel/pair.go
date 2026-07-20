package tunnel

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Pairing lets a user connect the abox-link app to their agentbox account by
// copying one string out of the browser, instead of typing a server URL,
// username and password into the client.
//
// The code carries the server address plus a short-lived, single-use secret.
// The app decodes the address, redeems the secret at that server, and gets a
// session token back — so the browser's own token is never handed over and a
// leaked code expires on its own.
const PairCodePrefix = "ABOX1-"

// PairPayload is what a pairing code encodes.
type PairPayload struct {
	Server string `json:"s"` // agentbox base URL, e.g. https://box.example.com
	Code   string `json:"c"` // single-use redemption secret
}

// EncodePairCode renders a payload as the string the user copies.
func EncodePairCode(server, code string) string {
	raw, _ := json.Marshal(PairPayload{Server: strings.TrimRight(server, "/"), Code: code})
	return PairCodePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// DecodePairCode parses a pasted pairing code. It tolerates the whitespace and
// line breaks that survive a copy-paste round trip.
func DecodePairCode(s string) (PairPayload, error) {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if !strings.HasPrefix(s, PairCodePrefix) {
		return PairPayload{}, fmt.Errorf("这不像一个配对码（应以 %s 开头）", PairCodePrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, PairCodePrefix))
	if err != nil {
		return PairPayload{}, fmt.Errorf("配对码格式错误，请重新复制")
	}
	var p PairPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Server == "" || p.Code == "" {
		return PairPayload{}, fmt.Errorf("配对码内容不完整，请重新生成")
	}
	return p, nil
}
