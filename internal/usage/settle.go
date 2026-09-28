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
	if len(t.evs) == 0 {
		return 0
	}
	evs := t.evs
	t.evs = nil // 防重复落库：同一个 tally 再 flush 一次不该再扣钱
	now := time.Now()
	plan := t.pricing
	if plan == nil {
		current := s.cfg.PricingPlan()
		plan = &current
	}
	for i := range evs {
		evs[i].TS = now
		evs[i].WallMS = wall.Milliseconds()
		// provider 不报价的行（codex）在这里按配置的价目表折算，落库前定好价，
		// 扣减才有依据。查不到价就是 0，只记不扣。
		evs[i] = priceWith(*plan, evs[i])
	}
	if err := s.store.InsertUsage(evs...); err != nil {
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
