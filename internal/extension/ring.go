package extension

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const ringSize = 200

// LogLine is one extension log entry, shown only to administrators.
type LogLine struct {
	Seq     int64     `json:"seq"`
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// Event records an interruption or rejection. It never contains prompts or
// model output; P1 keeps the latest 200 per extension in memory only.
type Event struct {
	Seq       int64              `json:"seq"`
	Time      time.Time          `json:"time"`
	Extension string             `json:"extension"`
	Kind      string             `json:"kind"` // abort | reject | unavailable | overloaded
	Code      string             `json:"code"`
	Stream    string             `json:"stream,omitempty"`
	Session   string             `json:"session,omitempty"`
	Account   string             `json:"account,omitempty"`
	User      string             `json:"user,omitempty"`
	Detail    map[string]float64 `json:"detail,omitempty"`
}

type ring[T any] struct {
	items []T
	next  int64
}

func (r *ring[T]) add(v T) {
	r.items = append(r.items, v)
	if len(r.items) > ringSize {
		r.items = append(r.items[:0:0], r.items[len(r.items)-ringSize:]...)
	}
}

// sanitizeLine keeps log text printable and bounded.
func sanitizeLine(s string, maxBytes int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}
