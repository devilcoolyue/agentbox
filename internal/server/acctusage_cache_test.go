package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/agent"
	"agentbox/internal/store"
)

// usageUpstream 起一个假的官方额度接口，handler 决定每次怎么回，返回打到的次数。
func usageUpstream(t *testing.T, handler func(w http.ResponseWriter, n int)) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, int(hits.Add(1)))
	}))
	t.Cleanup(up.Close)
	claudeOAuthUsage = up.URL
	t.Cleanup(func() { claudeOAuthUsage = "https://api.anthropic.com/api/oauth/usage" })
	return &hits
}

func getUsage(t *testing.T, s *Server, sess store.Session, refresh bool) (int, acctUsageView) {
	t.Helper()
	path := "/api/sessions/s1/account/usage"
	if refresh {
		path += "?refresh=1"
	}
	w := httptest.NewRecorder()
	s.handleAccountUsage(w, httptest.NewRequest(http.MethodGet, path, nil), sess)
	var v acctUsageView
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, v
}

func windowByKey(v acctUsageView, key string) (usageWindow, bool) {
	for _, w := range v.Windows {
		if w.Key == key {
			return w, true
		}
	}
	return usageWindow{}, false
}

// 对话响应报过窗口，打开弹窗就直接用它，不去碰限流很紧的官方接口。
func TestAccountUsageServesResponseWindowsWithoutUpstream(t *testing.T) {
	s, sess := newTestServer(t)
	acct := seedClaudeAccount(t, s, &sess, time.Now().Add(time.Hour).UnixMilli())
	hits := usageUpstream(t, func(w http.ResponseWriter, _ int) { fmt.Fprint(w, usageSample) })

	line := []byte(fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.3,"resetsAt":%d,"status":"allowed"},"seven_day":{"utilization":0.46,"resetsAt":%d}}}}`,
		time.Now().Add(2*time.Hour).Unix(), time.Now().Add(72*time.Hour).Unix()))
	ws, ok := agent.UsageWindows(line)
	if !ok {
		t.Fatal("rate_limit_event not read")
	}
	s.acctUsage.noteLocal(acct.ID, ws, time.Now())

	code, v := getUsage(t, s, sess, false)
	if code != http.StatusOK || hits.Load() != 0 {
		t.Fatalf("status %d, upstream hits %d; want 200 without upstream", code, hits.Load())
	}
	if v.Source != "responses" || v.Plan != "pro" || v.FetchedAt == "" {
		t.Errorf("view = %+v", v)
	}
	if w, ok := windowByKey(v, "five_hour"); !ok || w.Percent != 30 || w.ResetsAt == "" {
		t.Errorf("five_hour = %+v", w)
	}
	if w, ok := windowByKey(v, "seven_day"); !ok || w.Percent != 46 {
		t.Errorf("seven_day = %+v", w)
	}
}

// 刷新才打官方接口；一分钟内的重复刷新用缓存；之后的对话响应更新同名窗口，
// 官方接口独有的东西（额外用量）留着。
func TestAccountUsageRefreshCachesAndMerges(t *testing.T) {
	s, sess := newTestServer(t)
	acct := seedClaudeAccount(t, s, &sess, time.Now().Add(time.Hour).UnixMilli())
	hits := usageUpstream(t, func(w http.ResponseWriter, _ int) { fmt.Fprint(w, usageSample) })

	// 什么都没有时，不点刷新也得查一次。
	code, v := getUsage(t, s, sess, false)
	if code != http.StatusOK || hits.Load() != 1 || v.Source != "upstream" {
		t.Fatalf("status %d, hits %d, source %q", code, hits.Load(), v.Source)
	}
	if code, _ = getUsage(t, s, sess, true); code != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("refresh within a minute hit upstream again: status %d, hits %d", code, hits.Load())
	}
	if code, _ = getUsage(t, s, sess, false); hits.Load() != 1 {
		t.Fatalf("plain open hit upstream: hits %d", hits.Load())
	}

	s.acctUsage.noteLocal(acct.ID, map[string]agent.RateWindow{
		"five_hour": {Percent: 61, ResetsAt: time.Now().Add(time.Hour)},
	}, time.Now().Add(time.Second))
	_, v = getUsage(t, s, sess, false)
	if v.Source != "responses" {
		t.Errorf("source = %q, want responses", v.Source)
	}
	if w, _ := windowByKey(v, "five_hour"); w.Percent != 61 {
		t.Errorf("five_hour = %+v, want the newer response's 61", w)
	}
	if w, _ := windowByKey(v, "seven_day"); w.Percent != 4 {
		t.Errorf("seven_day = %+v, want upstream's 4 kept", w)
	}
	if v.Extra == nil || v.Extra.Used != 18.51 {
		t.Errorf("extra = %+v, want upstream's kept", v.Extra)
	}
	if v.Windows[0].Key != "five_hour" {
		t.Errorf("order = %+v", v.Windows)
	}
}

// 429 之后在 Retry-After 内不再打官方接口，照样返回手上的数据并说明冷却到几点。
func TestAccountUsageHonoursRetryAfter(t *testing.T) {
	s, sess := newTestServer(t)
	acct := seedClaudeAccount(t, s, &sess, time.Now().Add(time.Hour).UnixMilli())
	hits := usageUpstream(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Retry-After", "3056")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"type":"rate_limit_error"}}`)
	})

	// 什么都没有：200、空窗口、带冷却时间，页面据此说明。
	code, v := getUsage(t, s, sess, false)
	if code != http.StatusOK || len(v.Windows) != 0 || v.ThrottledUntil == "" {
		t.Fatalf("status %d, view %+v", code, v)
	}
	until, err := time.Parse(time.RFC3339, v.ThrottledUntil)
	if err != nil || until.Before(time.Now().Add(50*time.Minute)) {
		t.Errorf("throttled_until = %q, want ~51 minutes ahead", v.ThrottledUntil)
	}

	s.acctUsage.noteLocal(acct.ID, map[string]agent.RateWindow{
		"five_hour": {Percent: 12, ResetsAt: time.Now().Add(time.Hour)},
	}, time.Now())
	for range 3 {
		code, v = getUsage(t, s, sess, true)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (no retries while throttled)", hits.Load())
	}
	if code != http.StatusOK || v.Source != "responses" || len(v.Windows) != 1 || v.ThrottledUntil == "" || v.RefreshError != "" {
		t.Errorf("status %d, view %+v", code, v)
	}
}

// 其他失败（上游 5xx）有数据时降级成提示，没数据时照旧报错。
func TestAccountUsageRefreshErrorKeepsData(t *testing.T) {
	s, sess := newTestServer(t)
	acct := seedClaudeAccount(t, s, &sess, time.Now().Add(time.Hour).UnixMilli())
	usageUpstream(t, func(w http.ResponseWriter, _ int) { w.WriteHeader(http.StatusInternalServerError) })

	if code, _ := getUsage(t, s, sess, false); code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 with nothing to show", code)
	}
	s.acctUsage.noteLocal(acct.ID, map[string]agent.RateWindow{
		"five_hour": {Percent: 12, ResetsAt: time.Now().Add(time.Hour)},
	}, time.Now())
	code, v := getUsage(t, s, sess, true)
	if code != http.StatusOK || v.RefreshError == "" || len(v.Windows) != 1 || v.ThrottledUntil != "" {
		t.Errorf("status %d, view %+v", code, v)
	}
}

// 同一账号同时点刷新只打一次上游。
func TestAccountUsageConcurrentRefreshesShareOneRequest(t *testing.T) {
	var c acctUsageCache
	release := make(chan struct{})
	var calls atomic.Int32
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.fetchUpstream("a", time.Now, func() upstreamResult {
				calls.Add(1)
				<-release
				return upstreamResult{windows: []usageWindow{{Key: "five_hour", Percent: 1}}}
			})
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", calls.Load())
	}
	for _, err := range errs {
		if err != nil {
			t.Errorf("err = %v", err)
		}
	}
}

// 过了重置时间的窗口不再显示旧数字。
func TestMergeUsageDropsWindowsPastReset(t *testing.T) {
	now := time.Now()
	st := usageState{local: &usageSnapshot{at: now, windows: map[string]agent.RateWindow{
		"five_hour": {Percent: 99, ResetsAt: now.Add(-time.Minute)},
		"seven_day": {Percent: 40, ResetsAt: now.Add(time.Hour)},
	}}}
	windows, _, _, _, ok := mergeUsage(st, now)
	if !ok || len(windows) != 1 || windows[0].Key != "seven_day" {
		t.Errorf("windows = %+v", windows)
	}
}

func TestUsageRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"3056", 3056 * time.Second},
		{"", usageUpstreamDefaultBackoff},
		{"junk", usageUpstreamDefaultBackoff},
		{"99999", usageUpstreamMaxBackoff},
		{now.Add(10 * time.Minute).Format(http.TimeFormat), 10 * time.Minute},
	} {
		h := http.Header{}
		if tc.value != "" {
			h.Set("Retry-After", tc.value)
		}
		if got := usageRetryAfter(h, now); got != tc.want {
			t.Errorf("Retry-After %q = %v, want %v", tc.value, got, tc.want)
		}
	}
}
