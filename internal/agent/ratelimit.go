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
