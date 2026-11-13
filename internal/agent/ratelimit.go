package agent

import (
	"encoding/json"
	"math"
	"time"
)

// RateLimitType is the stream event in which Claude Code passes on the
// subscription limits the provider reported with a model response.
const RateLimitType = "rate_limit_event"

// RateWindow is a subscription's five-hour usage window as of one model
// response: how much of it is used and when it starts over.
type RateWindow struct {
	Percent  float64 // 0-100
	ResetsAt time.Time
}

// FiveHourWindow reads the five-hour window from a Claude rate_limit_event.
// Claude Code 2.1.3xx reports every window under unifiedWindows with a 0-1
// utilization; without it, a rejection of the five-hour window still says it
// is used up.
func FiveHourWindow(line []byte) (RateWindow, bool) {
	var ev struct {
		Type string `json:"type"`
		Info struct {
			Status        string `json:"status"`
			RateLimitType string `json:"rateLimitType"`
			ResetsAt      int64  `json:"resetsAt"`
			Windows       struct {
				FiveHour *struct {
					Utilization *float64 `json:"utilization"`
					ResetsAt    int64    `json:"resetsAt"`
				} `json:"five_hour"`
			} `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Type != RateLimitType {
		return RateWindow{}, false
	}
	if w := ev.Info.Windows.FiveHour; w != nil && w.Utilization != nil && w.ResetsAt > 0 {
		// Rounded so 0.95 compares as 95, not 94.99999999999999.
		percent := math.Round(*w.Utilization*10000) / 100
		return RateWindow{Percent: min(max(percent, 0), 100), ResetsAt: time.Unix(w.ResetsAt, 0)}, true
	}
	if ev.Info.Status == "rejected" && ev.Info.RateLimitType == "five_hour" && ev.Info.ResetsAt > 0 {
		return RateWindow{Percent: 100, ResetsAt: time.Unix(ev.Info.ResetsAt, 0)}, true
	}
	return RateWindow{}, false
}

// UsageWindows reads every window a Claude rate_limit_event reported under
// unifiedWindows, keyed the way the CLI names them (five_hour, seven_day, and
// any model-scoped weekly window). These are the numbers the CLI hands its
// status line: they ride on every model response, so reading them costs no
// request of the rate-limited usage endpoint.
func UsageWindows(line []byte) (map[string]RateWindow, bool) {
	var ev struct {
		Type string `json:"type"`
		Info struct {
			Windows map[string]*struct {
				Utilization *float64 `json:"utilization"`
				ResetsAt    int64    `json:"resetsAt"`
			} `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Type != RateLimitType {
		return nil, false
	}
	out := map[string]RateWindow{}
	for key, w := range ev.Info.Windows {
		// Bounded: the stream is the CLI's, but keys end up in API responses.
		if w == nil || w.Utilization == nil || w.ResetsAt <= 0 || key == "" || len(key) > 64 || len(out) >= 16 {
			continue
		}
		percent := math.Round(*w.Utilization*10000) / 100
		out[key] = RateWindow{Percent: min(max(percent, 0), 100), ResetsAt: time.Unix(w.ResetsAt, 0)}
	}
	return out, len(out) > 0
}
