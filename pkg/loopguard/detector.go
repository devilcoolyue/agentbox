// Package loopguard detects repetitive model output without depending on an
// agent harness. Each Detector belongs to one text or reasoning stream.
package loopguard

import (
	"strings"
	"unicode"
)

// DefaultWindow and DefaultDistinctLimit are the detection thresholds used by
// a zero Detector: the last 256 short sentences containing at most 24 distinct
// phrases trip the detector.
const DefaultWindow = 256
const DefaultDistinctLimit = 24

const maxUnitRunes = 160

// Detection deliberately contains no model output or prompt text.
type Detection struct {
	Channel  string `json:"channel"`
	Window   int    `json:"window"`
	Distinct int    `json:"distinct"`
}

// Detector has bounded memory. Its zero value is ready to use with the default
// thresholds. It recognizes low-diversity runs of short sentences, including
// shuffled phrase pools, rather than identical transport chunks. Long prose and
// code reset the candidate run.
type Detector struct {
	window   int
	distinct int
	unit     []rune
	stop     bool
	over     bool
	fenced   bool
	ring     []string
	counts   map[string]int
	n, next  int
	tripped  *Detection
}

// NewDetector uses window sentences and at most distinctLimit distinct ones.
// Values <= 0 select the defaults.
func NewDetector(window, distinctLimit int) *Detector {
	return &Detector{window: window, distinct: distinctLimit}
}

func (d *Detector) init() {
	if d.ring != nil {
		return
	}
	if d.window <= 0 {
		d.window = DefaultWindow
	}
	if d.distinct <= 0 {
		d.distinct = DefaultDistinctLimit
	}
	d.ring = make([]string, d.window)
}

// Push consumes decoded text deltas; callers must not feed a final full-text
// snapshot again after its deltas. Reasoning and visible text need separate
// detectors, as do parallel candidates and independent requests.
func (d *Detector) Push(delta string) *Detection {
	if d.tripped != nil {
		return d.tripped
	}
	d.init()
	for _, r := range delta {
		if r == '\n' || (d.stop && unicode.IsSpace(r)) {
			if hit := d.end(); hit != nil {
				return hit
			}
			continue
		}
		if !d.over {
			d.unit = append(d.unit, r)
			if len(d.unit) > maxUnitRunes {
				d.over = true
				d.reset()
			}
		}
		d.stop = strings.ContainsRune(".!?", r)
		if strings.ContainsRune("。！？", r) {
			if hit := d.end(); hit != nil {
				return hit
			}
		}
	}
	return nil
}

func (d *Detector) reset() {
	d.n, d.next = 0, 0
	clear(d.ring)
	clear(d.counts)
}

func (d *Detector) end() *Detection {
	s := strings.TrimSpace(string(d.unit))
	over := d.over
	d.unit, d.stop, d.over = d.unit[:0], false, false
	if strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~") {
		d.fenced = !d.fenced
		d.reset()
		return nil
	}
	if d.fenced || over {
		d.reset()
		return nil
	}
	if s == "" {
		return nil
	}
	letter := false
	for _, r := range s {
		letter = letter || unicode.IsLetter(r)
		// Conservative prose-only policy: repeated code, rows and numbered
		// progress logs must not become a candidate just because they repeat.
		if unicode.IsDigit(r) || strings.ContainsRune("`~{}[]()=<>/\\|_*+#$@", r) {
			d.reset()
			return nil
		}
	}
	if !letter {
		d.reset()
		return nil
	}
	s = strings.TrimRightFunc(strings.ToLower(s), func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(".!?。！？,，;；:：…", r)
	})
	s = strings.Join(strings.Fields(s), " ")
	if d.counts == nil {
		d.counts = make(map[string]int)
	}
	if d.n == d.window {
		old := d.ring[d.next]
		d.counts[old]--
		if d.counts[old] == 0 {
			delete(d.counts, old)
		}
	} else {
		d.n++
	}
	d.ring[d.next] = s
	d.next = (d.next + 1) % d.window
	d.counts[s]++
	if d.n == d.window && len(d.counts) <= d.distinct {
		d.tripped = &Detection{Window: d.window, Distinct: len(d.counts)}
	}
	return d.tripped
}
