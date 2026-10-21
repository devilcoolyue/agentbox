package usage

import (
	"log"
	"time"

	"agentbox/internal/store"
)

// NewTally pins the whole table, including prices for models used by subagents.
func (s *Service) NewTally() Tally {
	plan := s.cfg.PricingPlan()
	return Tally{pricing: &plan}
}

// flush 把汇总好的用量定价后落库，返回本回合记下的行数（含回合中已当场结算
// 的）。落库同时会从用户额度里扣掉这笔钱（store.InsertUsage 同事务完成）。
// 尽力而为：写库失败只记日志，绝不影响正在进行的对话。汇总为空（没认出用量
// 事件）时返回 0。
//
// wall 是本回合在我们这边的墙钟耗时，只有到这里回合才真的结束、才量得到，所以
// 由调用方在 flush 的时刻算好传进来（见 chat.go 的 defer）。0 表示没量。
func (s *Service) Flush(t *Tally, wall time.Duration) int {
	if t.claude != nil {
		return s.flushClaudeMessages(t, wall)
	}
	if len(t.evs) == 0 && len(t.codexTurns) == 0 && len(t.codexLive) == 0 && len(t.codexRetry) == 0 {
		return 0
	}
	now := time.Now()
	plan := t.pricing
	if plan == nil {
		current := s.cfg.PricingPlan()
		plan = &current
	}
	// 回合中已经当场结算的请求（settleCodexRequest），收尾展开时按 raw 认出跳过；
	// 回合第一次请求的回合级耗时只有现在才有，补到已记的那一行上。
	live := make(map[string]store.UsageEvent, len(t.codexLive))
	for _, row := range t.codexLive {
		live[row.Raw] = row
	}
	var evs, finish []store.UsageEvent
	seen := map[string]bool{}
	for _, ev := range t.evs {
		ev.TS = now
		ev.WallMS = wall.Milliseconds()
		// app-server 的 Codex 回合逐次请求成行，各自查价判档。provider 不报价的
		// 行（codex）在这里按配置的价目表折算，落库前定好价，扣减才有依据。
		// 查不到价就是 0，只记不扣。
		for _, row := range expandCodexRequests(ev) {
			seen[row.Raw] = true
			if done, ok := live[row.Raw]; ok {
				if row.TTFTMs != 0 || row.DurationMS != 0 || row.WallMS != 0 {
					done.TTFTMs, done.DurationMS, done.WallMS = row.TTFTMs, row.DurationMS, row.WallMS
					finish = append(finish, done)
				}
				continue
			}
			evs = append(evs, priceWith(*plan, row))
		}
	}
	// 当场写库失败、收尾事件又没再列出的（进程没走到收尾），照原样重记。
	for _, row := range t.codexRetry {
		if !seen[row.Raw] {
			evs = append(evs, row)
		}
	}
	// 远程压缩那次请求不在收尾事件的用量里，从会话记录补（见 codex_compaction.go）。
	recorded := append(append([]store.UsageEvent(nil), evs...), t.codexLive...)
	for _, row := range s.compactionRows(t, recorded) {
		if row.TS.IsZero() {
			row.TS = now
		}
		evs = append(evs, priceWith(*plan, row))
	}
	settled := len(t.codexLive)
	// 防重复落库：同一个 tally 再 flush 一次不该再扣钱
	t.evs, t.codexTurns, t.codexLive, t.codexRetry = nil, nil, nil, nil
	if err := s.store.FinishUsageRows(finish...); err != nil {
		log.Printf("usage: 会话 %s 补记回合耗时: %v", finish[0].SessionID, err)
	}
	if len(evs) == 0 {
		return settled
	}
	if err := s.store.InsertUsage(evs...); err != nil {
		t.settlementErr = err
		log.Printf("record usage %s: %v", evs[0].SessionID, err)
		return settled
	}
	return settled + len(evs)
}

// recordUsage 记一趟自成一体的消耗（起标题）：只有一行收尾事件，收下就落库。
func (s *Service) Record(base store.UsageEvent, line []byte, wall time.Duration) int {
	var t Tally
	t.Observe(base, line)
	return s.Flush(&t, wall)
}
