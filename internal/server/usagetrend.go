package server

// 使用趋势：使用记录页的时序柱状图。筛选口径与 /usage/events 共用 usageFilterFrom，
// 可见范围同样钉死，柱子加起来等于同一筛选下的合计。
//
// 分桶思路取自 cc-switch 的 get_daily_trends：范围短按小时、长按天，没有消耗的时段
// 也补一个空桶，时间轴才不会把两段相隔很远的消耗画成紧挨着。这里多做了三件事：
//   - 桶按系统时区（config.timezone）的钟点和日历切，不按范围起点数整小时，滚动范围
//     的柱子也对得上表格里的时刻；
//   - 范围上限落在未来时（「当天」截到 23:59）只画到现在，后面的空位不是零消耗；
//   - 「全部」或起点早于数据时从第一条记录开始；超过半年按月，柱子不至于细到看不见。

import (
	"log"
	"net/http"
	"time"

	"agentbox/internal/store"
)

const (
	trendHour  = "hour"
	trendDay   = "day"
	trendMonth = "month"

	// 两天内按小时：「24 小时」预设和夏令时的 25 小时一天都落在这里。
	trendHourlyMax = 48 * time.Hour
	trendDailyMax  = 186 * 24 * time.Hour
)

// usageTrendBucket 是图上的一根柱子。Start 是系统时区下的墙上时间
// （usageLocalLayout），前端不做时区换算；金额同样是微美元整数。
type usageTrendBucket struct {
	Start            string `json:"start"`
	Requests         int    `json:"requests"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	CostMicroUSD     int64  `json:"cost_micro_usd"`
}

func (s *Server) handleUsageTrend(w http.ResponseWriter, r *http.Request) {
	f, _ := s.usageFilterFrom(r)
	slots, err := s.store.UsageSlots(f)
	if err != nil {
		log.Printf("usage: 读取使用趋势失败: %v", err)
		writeErr(w, http.StatusInternalServerError, "读取使用趋势失败")
		return
	}
	granularity, buckets := foldUsageTrend(slots, f.Since, f.Until, time.Now(), s.cfg.GetLocation())
	writeJSON(w, http.StatusOK, map[string]any{
		"granularity": granularity,
		"buckets":     buckets,
		"timezone":    s.cfg.GetTimeZone(),
	})
}

// foldUsageTrend 把按时间排好的 UTC 刻钟槽折叠成连续的本地时区桶。没有任何记录时
// 不出桶：页面显示空状态，一段空轴没有信息，也省得为任意大的时间范围铺桶。
func foldUsageTrend(slots []store.UsageSlot, since, until, now time.Time, loc *time.Location) (string, []usageTrendBucket) {
	if len(slots) == 0 {
		return trendHour, []usageTrendBucket{}
	}
	end := now
	if !until.IsZero() && until.Before(end) {
		end = until
	}
	// 时钟偏差可能留下「未来」的行：它们也在合计里，所以轴要延伸到能装下它们。
	if last := slots[len(slots)-1].Start.Add(store.UsageSlotSize); last.After(end) {
		end = last
	}
	start := since
	if start.IsZero() || end.Sub(start) > trendDailyMax {
		start = slots[0].Start
	}

	granularity, next := trendMonth, nextLocalMonth
	first := time.Date(start.In(loc).Year(), start.In(loc).Month(), 1, 0, 0, 0, 0, loc)
	switch span := end.Sub(start); {
	case span <= trendHourlyMax:
		granularity, next, first = trendHour, nextLocalHour, floorLocalHour(start.In(loc))
	case span <= trendDailyMax:
		y, m, d := start.In(loc).Date()
		granularity, next, first = trendDay, nextLocalDay, time.Date(y, m, d, 0, 0, 0, 0, loc)
	}

	var starts []time.Time
	buckets := []usageTrendBucket{}
	for t := first; t.Before(end); t = next(t) {
		starts = append(starts, t)
		buckets = append(buckets, usageTrendBucket{Start: t.Format(usageLocalLayout)})
	}
	// 按序双指针而不是查表：每个槽都一定落进某个桶，柱子之和永远等于合计。
	i := 0
	for _, slot := range slots {
		for i+1 < len(starts) && !slot.Start.Before(starts[i+1]) {
			i++
		}
		b := &buckets[i]
		b.Requests += slot.Requests
		b.InputTokens += slot.InputTokens
		b.OutputTokens += slot.OutputTokens
		b.CacheReadTokens += slot.CacheReadTokens
		b.CacheWriteTokens += slot.CacheWriteTokens
		b.CostMicroUSD += slot.CostMicroUSD
	}
	return granularity, buckets
}

// floorLocalHour 按本地钟点取整。Truncate 只认绝对时间，半小时时区要先平移偏移量。
func floorLocalHour(t time.Time) time.Time {
	_, offset := t.Zone()
	shift := time.Duration(offset) * time.Second
	return t.Add(shift).Truncate(time.Hour).Add(-shift)
}

// 走绝对时间再取整：夏令时切换当天的桶数才对（多出或少掉一个钟点）。
func nextLocalHour(t time.Time) time.Time { return floorLocalHour(t.Add(time.Hour)) }

func nextLocalDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, t.Location())
}

func nextLocalMonth(t time.Time) time.Time {
	y, m, _ := t.Date()
	return time.Date(y, m+1, 1, 0, 0, 0, 0, t.Location())
}
