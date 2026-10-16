package modelgw

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MaxEventBytes bounds one SSE event, including a malicious line without a
// newline.
const MaxEventBytes = 1 << 20

// MaxRequestBytes bounds an inspected POST body.
const MaxRequestBytes = 32 << 20

var errEventLimit = errors.New("event limit")

// ReadEvent preserves upstream bytes, including CRLF, comments and multi-line
// data fields. Its bound applies even to a malicious line without a newline.
func ReadEvent(r *bufio.Reader) ([]byte, error) {
	var event []byte
	lineStart := 0
	for {
		// ReadByte still uses the buffered reader. ReadSlice could wait for a
		// whole additional buffer after the limit, leaving a too-large event
		// running if the provider pauses before its next newline.
		b, err := r.ReadByte()
		if err != nil {
			return event, err
		}
		if len(event) == MaxEventBytes {
			return nil, errEventLimit
		}
		event = append(event, b)
		if b != '\n' {
			continue
		}
		line := event[lineStart:]
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			return event, nil
		}
		lineStart = len(event)
	}
}

// FingerprintBody hashes JSON structurally because Codex reserializes nested
// objects in a different key order on reconnect. The original bytes are still
// what goes upstream. UseNumber preserves large integers without float64
// rounding collisions. Non-JSON input is returned unchanged.
func FingerprintBody(body []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return body
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return body
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return normalized
}

// Fingerprint identifies a request without retaining credentials or prompts:
// request URI, the provider credential headers and the normalized body.
func Fingerprint(r *http.Request, body []byte) [32]byte {
	h := sha256.New()
	for _, value := range []string{r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("X-Goog-Api-Key")} {
		fmt.Fprintf(h, "%d:", len(value))
		io.WriteString(h, value)
	}
	h.Write(FingerprintBody(body))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// StripHopHeaders removes hop-by-hop headers, including any named in
// Connection.
func StripHopHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, k := range strings.Split(value, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}

// WriteError writes the provider-style JSON error body shared by every
// gateway rejection.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": code, "code": code, "message": message}})
}
