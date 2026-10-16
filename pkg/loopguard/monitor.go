package loopguard

import "agentbox/pkg/modelgw"

// ErrorCode is reported when a stream is interrupted for repeating itself.
const ErrorCode = "model_output_loop"

// maxChannels bounds exotic upstreams with unbounded item IDs. Eviction only
// loses detection history; it cannot combine unrelated candidates.
const maxChannels = 128

// Monitor follows every text channel of one response stream, with a separate
// Detector per (part, channel). It is not safe for concurrent use.
type Monitor struct {
	window, distinct int
	detectors        map[string]*Detector
}

// NewMonitor uses the given thresholds for each channel; values <= 0 select
// the defaults.
func NewMonitor(window, distinctLimit int) *Monitor {
	return &Monitor{window: window, distinct: distinctLimit}
}

// Push consumes decoded deltas. Ended parts release their detectors first, so
// a closed content block never combines with the next one.
func (m *Monitor) Push(deltas []modelgw.Delta, ended []string) *Detection {
	for _, part := range ended {
		delete(m.detectors, part+":text")
		delete(m.detectors, part+":reasoning")
	}
	for _, delta := range deltas {
		if delta.Text == "" {
			continue
		}
		if m.detectors == nil {
			m.detectors = make(map[string]*Detector)
		}
		key := delta.Part + ":" + delta.Channel
		d := m.detectors[key]
		if d == nil {
			if len(m.detectors) >= maxChannels {
				clear(m.detectors)
			}
			d = NewDetector(m.window, m.distinct)
			m.detectors[key] = d
		}
		if hit := d.Push(delta.Text); hit != nil {
			result := *hit
			result.Channel = delta.Channel
			return &result
		}
	}
	return nil
}
