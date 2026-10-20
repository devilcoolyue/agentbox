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

// flush 把汇总好的用量定价后落库，返回写进去的行数。落库同时会从用户额度里扣
// 掉这笔钱（store.InsertUsage 同事务完成）。尽力而为：写库失败只记日志，绝不
// 影响正在进行的对话。汇总为空（没认出用量事件）时返回 0。
//
// wall 是本回合在我们这边的墙钟耗时，只有到这里回合才真的结束、才量得到，所以
// 由调用方在 flush 的时刻算好传进来（见 chat.go 的 defer）。0 表示没量。
func (s *Service) Flush(t *Tally, wall time.Duration) int {
	if t.claude != nil {
		return s.flushClaudeMessages(t, wall)
	}
	if len(t.evs) == 0 && len(t.codexTurns) == 0 {
		return 0
	}
	now := time.Now()
	plan := t.pricing
	if plan == nil {
		current := s.cfg.PricingPlan()
		plan = &current
	}
	var evs []store.UsageEvent
	for _, ev := range t.evs {
		ev.TS = now
		ev.WallMS = wall.Milliseconds()
		// app-server 的 Codex 回合逐次请求成行，各自查价判档。provider 不报价的
		// 行（codex）在这里按配置的价目表折算，落库前定好价，扣减才有依据。
		// 查不到价就是 0，只记不扣。
		for _, row := range expandCodexRequests(ev) {
			evs = append(evs, priceWith(*plan, row))
		}
	}
	// 远程压缩那次请求不在收尾事件的用量里，从会话记录补（见 codex_compaction.go）。
	for _, row := range s.compactionRows(t, evs) {
		if row.TS.IsZero() {
			row.TS = now
		}
		evs = append(evs, priceWith(*plan, row))
	}
	t.evs, t.codexTurns = nil, nil // 防重复落库：同一个 tally 再 flush 一次不该再扣钱
	if len(evs) == 0 {
		return 0
	}
	if err := s.store.InsertUsage(evs...); err != nil {
		t.settlementErr = err
		log.Printf("record usage %s: %v", evs[0].SessionID, err)
		return 0
	}
	return len(evs)
}

// recordUsage 记一趟自成一体的消耗（起标题）：只有一行收尾事件，收下就落库。
func (s *Service) Record(base store.UsageEvent, line []byte, wall time.Duration) int {
	var t Tally
	t.Observe(base, line)
	return s.Flush(&t, wall)
}
