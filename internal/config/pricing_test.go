package config

import "testing"

// 价目直接决定扣多少钱，畸形值必须整表拒掉：负单价会变成「用得越多余额越多」，
// 长上下文档缺阈值则永远命中不到、安静失效。
func TestSanitizePricing(t *testing.T) {
	ok := TokenRates{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 10}

	cases := []struct {
		name    string
		in      map[string]ModelPrice
		wantErr bool
	}{
		{"正常一行", map[string]ModelPrice{"claude-opus-5": {TokenRates: ok}}, false},
		{"agent 兜底价", map[string]ModelPrice{"codex": {TokenRates: ok}}, false},
		{"全 0 = 免费，合法", map[string]ModelPrice{"x": {}}, false},
		{
			"完整的长上下文档",
			map[string]ModelPrice{"gpt-5.5": {TokenRates: ok, LongContextOver: 272000, Long: &TokenRates{Input: 10, Output: 45}}},
			false,
		},
		{"负单价", map[string]ModelPrice{"x": {TokenRates: TokenRates{Input: -1}}}, true},
		{"单价大得离谱（多打了几个零）", map[string]ModelPrice{"x": {TokenRates: TokenRates{Input: 1e9}}}, true},
		{
			"配了长上下文价却没阈值",
			map[string]ModelPrice{"x": {TokenRates: ok, Long: &TokenRates{Input: 10}}},
			true,
		},
		{"负阈值", map[string]ModelPrice{"x": {TokenRates: ok, LongContextOver: -1}}, true},
		{"空键", map[string]ModelPrice{"": {TokenRates: ok}}, true},
		{"键里有空格", map[string]ModelPrice{"claude opus": {TokenRates: ok}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := sanitizePricing(c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v，想要 wantErr=%v", err, c.wantErr)
			}
		})
	}
}

// 只填阈值而不填单价，等于没配价：得留在表里但不生成长上下文档，
// 否则 Rates 会在超阈值时返回一整档 0，变成「越长越免费」。
func TestSanitizePricingKeepsThresholdWithoutLongTier(t *testing.T) {
	out, err := sanitizePricing(map[string]ModelPrice{
		"x": {TokenRates: TokenRates{Input: 5}, LongContextOver: 272000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["x"].Long != nil {
		t.Error("没填单价却生成了长上下文档")
	}
}

// provider 报的模型 id 常带日期快照（claude-haiku-4-5-20251001），而价目表里
// 配的是不带日期的系列名。查不到就等于「未定价」，一分钱都扣不出来。
func TestPriceFallsBackToUndatedModel(t *testing.T) {
	c := &Config{Pricing: map[string]ModelPrice{
		"claude-haiku-4-5": {TokenRates: TokenRates{Input: 1, Output: 5}},
		"claude-opus-5":    {TokenRates: TokenRates{Input: 5, Output: 25}},
		"codex":            {TokenRates: TokenRates{Input: 5, Output: 30}},
	}}

	cases := []struct {
		agent, model string
		wantInput    float64
		wantOK       bool
	}{
		{"claude", "claude-opus-5", 5, true},             // 精确命中
		{"claude", "claude-haiku-4-5-20251001", 1, true}, // 去掉日期后缀命中
		{"codex", "", 5, true},                           // 事件没报模型，落到 agent 兜底
		{"codex", "gpt-5.5-2026-01-01", 5, true},         // 不是 8 位日期，落到 agent 兜底
		{"claude", "claude-sonnet-4-6", 0, false},        // 表里没有，也没有 claude 兜底
	}
	for _, c2 := range cases {
		got, ok := c.Price(c2.agent, c2.model)
		if ok != c2.wantOK || got.Input != c2.wantInput {
			t.Errorf("Price(%q, %q) = %v/%v，想要 %v/%v",
				c2.agent, c2.model, got.Input, ok, c2.wantInput, c2.wantOK)
		}
	}
}
