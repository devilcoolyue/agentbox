package server

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"agentbox/internal/agent"
)

// 账号额度的两个来源。
//
// 一是对话响应：Claude Code 每次模型请求的响应头都带着订阅窗口的用量，CLI 以
// rate_limit_event 转给我们（网页对话、保活都有），也是 CLI 状态栏 / claude-hud
// 显示的那份。不花任何额外请求，按账号记最新一份即可。
//
// 二是官方 /api/oauth/usage：多出按模型的周额度、额外用量，但这个接口限流很紧
// （同一账号十分钟内几次就 429，Retry-After 常在一小时上下），而且计数和账号的
// 其他调用方（终端里的 /usage、本机的 CLI 与工具）共享。所以只在用户点刷新、
// 或手上什么数据都没有时才打；成功结果按账号缓存，同账号并发的刷新合并成一次，
// 429 之后在 Retry-After 内不再打，期间照常返回已有的数据。
//
// 都只在内存里：重启后第一次打开弹窗退回官方接口一次，换来不动数据库。

// usageUpstreamMinInterval 内重复刷新直接用上次的结果。
const usageUpstreamMinInterval = time.Minute

// 429 没带可用 Retry-After 时的冷却，以及冷却的上限。
const (
	usageUpstreamDefaultBackoff = 15 * time.Minute
	usageUpstreamMaxBackoff     = 2 * time.Hour
)

// usageSnapshot 是对话响应报的窗口，at 是最近一次更新的时间。
type usageSnapshot struct {
	windows map[string]agent.RateWindow
	at      time.Time
}

// upstreamUsage 是一个账号在官方额度接口上的状态。
type upstreamUsage struct {
	windows      []usageWindow // 上次成功的结果；nil 表示没成功过
	extra        *usageExtra
	at           time.Time
	blockedUntil time.Time     // 429 冷却到什么时候
	inflight     chan struct{} // 非 nil 表示正在查，查完关闭
}

type acctUsageCache struct {
	mu       sync.Mutex
	local    map[string]*usageSnapshot
	upstream map[string]*upstreamUsage
}

// noteLocal 记下对话响应报的窗口。同一事件可能只带部分窗口（旧版 CLI 只有
// five_hour），所以按键合并，已经过了重置时间的旧窗口顺手丢掉。
func (c *acctUsageCache) noteLocal(account string, ws map[string]agent.RateWindow, now time.Time) {
	if account == "" || len(ws) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.local == nil {
		c.local = map[string]*usageSnapshot{}
	}
	snap := c.local[account]
	if snap == nil {
		snap = &usageSnapshot{windows: map[string]agent.RateWindow{}}
		c.local[account] = snap
	}
	for k, w := range snap.windows {
		if !w.ResetsAt.After(now) {
			delete(snap.windows, k)
		}
	}
	for k, w := range ws {
		snap.windows[k] = w
	}
	snap.at = now
}

// usageState 是合并视图需要的两份数据的拷贝。
type usageState struct {
	local    *usageSnapshot
	upstream upstreamUsage
}

func (c *acctUsageCache) state(account string) usageState {
	c.mu.Lock()
	defer c.mu.Unlock()
	var st usageState
	if snap := c.local[account]; snap != nil {
		cp := usageSnapshot{windows: make(map[string]agent.RateWindow, len(snap.windows)), at: snap.at}
		for k, w := range snap.windows {
			cp.windows[k] = w
		}
		st.local = &cp
	}
	if up := c.upstream[account]; up != nil {
		st.upstream = *up
		st.upstream.inflight = nil
	}
	return st
}

// upstreamResult 是打一次官方接口的结果。
type upstreamResult struct {
	windows    []usageWindow
	extra      *usageExtra
	retryAfter time.Duration // >0 表示 429
	err        error         // 429 以外的失败
}

// errUsageThrottled 表示官方接口在冷却中，这次没有去打。
type errUsageThrottled struct{ until time.Time }

func (e errUsageThrottled) Error() string { return "usage endpoint throttled" }

// fetchUpstream 在不冷却、也没有新鲜缓存时调用 do 打一次官方接口；同账号并发的
// 调用只有一个真正去打，其余等它的结果。返回 nil 表示缓存里已经是可用结果。
func (c *acctUsageCache) fetchUpstream(account string, now func() time.Time, do func() upstreamResult) error {
	for {
		c.mu.Lock()
		if c.upstream == nil {
			c.upstream = map[string]*upstreamUsage{}
		}
		up := c.upstream[account]
		if up == nil {
			up = &upstreamUsage{}
			c.upstream[account] = up
		}
		t := now()
		if t.Before(up.blockedUntil) {
			until := up.blockedUntil
			c.mu.Unlock()
			return errUsageThrottled{until}
		}
		if up.windows != nil && t.Sub(up.at) < usageUpstreamMinInterval {
			c.mu.Unlock()
			return nil
		}
		if wait := up.inflight; wait != nil {
			// 有人正在查：等它查完再看一圈，成功就命中缓存，429 就看到冷却。
			c.mu.Unlock()
			<-wait
			continue
		}
		up.inflight = make(chan struct{})
		c.mu.Unlock()

		res := do()

		c.mu.Lock()
		done := now()
		switch {
		case res.retryAfter > 0:
			up.blockedUntil = done.Add(res.retryAfter)
		case res.err == nil:
			up.windows, up.extra, up.at = res.windows, res.extra, done
		}
		close(up.inflight)
		up.inflight = nil
		until := up.blockedUntil
		c.mu.Unlock()
		switch {
		case res.retryAfter > 0:
			return errUsageThrottled{until}
		case res.err != nil:
			return res.err
		}
		return nil
	}
}

// usageRetryAfter 读 429 的 Retry-After（秒数或 HTTP 日期），读不出来用默认冷却，
// 并夹在上限以内。
func usageRetryAfter(h http.Header, now time.Time) time.Duration {
	d := usageUpstreamDefaultBackoff
	if v := h.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			d = time.Duration(n) * time.Second
		} else if at, err := http.ParseTime(v); err == nil && at.After(now) {
			d = at.Sub(now)
		}
	}
	return min(d, usageUpstreamMaxBackoff)
}

// usageWindowOrder 是窗口的展示顺序，认不出的键排在后面按字母序。
var usageWindowOrder = map[string]int{"five_hour": 0, "seven_day": 1, "seven_day_opus": 2, "seven_day_sonnet": 3}

func usageWindowLabel(key string) string {
	for _, spec := range usageWindowLabels {
		if spec.key == key {
			return spec.label
		}
	}
	return key
}

// mergeUsage 把两份数据合成一个视图：同一个窗口取较新那份；已经过了重置时间
// 的窗口不显示（新窗口的用量要等下一次响应才知道，不能拿旧数字充数）。
// 返回的 source 与 at 是较新那份的来源与时间；两份都没有时 ok 为 false。
func mergeUsage(st usageState, now time.Time) (windows []usageWindow, extra *usageExtra, source string, at time.Time, ok bool) {
	byKey := map[string]usageWindow{}
	addUpstream := func() {
		for _, w := range st.upstream.windows {
			byKey[w.Key] = w
		}
	}
	addLocal := func() {
		for k, w := range st.local.windows {
			byKey[k] = usageWindow{Key: k, Label: usageWindowLabel(k), Percent: w.Percent, ResetsAt: w.ResetsAt.UTC().Format(time.RFC3339)}
		}
	}
	hasUp, hasLocal := st.upstream.windows != nil, st.local != nil && len(st.local.windows) > 0
	switch {
	case hasLocal && (!hasUp || st.local.at.After(st.upstream.at)):
		if hasUp {
			addUpstream()
		}
		addLocal()
		source, at = "responses", st.local.at
	case hasUp:
		if hasLocal {
			addLocal()
		}
		addUpstream()
		source, at = "upstream", st.upstream.at
	default:
		return nil, nil, "", time.Time{}, false
	}
	windows = []usageWindow{}
	for _, w := range byKey {
		if w.ResetsAt != "" {
			if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil && !t.After(now) {
				continue
			}
		}
		windows = append(windows, w)
	}
	sort.Slice(windows, func(i, j int) bool {
		oi, iok := usageWindowOrder[windows[i].Key]
		oj, jok := usageWindowOrder[windows[j].Key]
		switch {
		case iok && jok:
			return oi < oj
		case iok != jok:
			return iok
		}
		return windows[i].Key < windows[j].Key
	})
	return windows, st.upstream.extra, source, at, true
}
