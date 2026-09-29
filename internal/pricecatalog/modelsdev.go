package pricecatalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentbox/internal/config"
)

const ModelsDevURL = "https://models.dev/api.json"
const ModelsDevMaxBytes = 16 << 20

// models.dev is a third-party feed, not an official or human-verified quote.
// Keep its raw cost fields so an unknown billing dimension cannot disappear
// silently during decoding. Model metadata may evolve independently.
type devModel struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Modalities struct {
		Output []string `json:"output"`
	} `json:"modalities"`
	Limit struct {
		Context int64 `json:"context"`
	} `json:"limit"`
	Cost map[string]json.RawMessage `json:"cost"`
}

var claudeVersion = regexp.MustCompile(`^claude-[a-z]+-(\d+)(?:-(\d{1,2}))?(?:-|$)`)
var openAIReasoning = regexp.MustCompile(`^o[134](?:-|$)`)
var legacyOpenAI = regexp.MustCompile(`^(gpt-(?:4(?:[.o-]|$)|5(?:\.[1-5])?(?:-|$))|o[134](?:-|$))`)

func devNumber(fields map[string]json.RawMessage, key string) (float64, error) {
	raw := fields[key]
	if len(raw) == 0 || string(raw) == "null" {
		return 0, fmt.Errorf("缺少 %s 单价", key)
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 10000 {
		return 0, fmt.Errorf("%s 单价无效", key)
	}
	return value, nil
}

func devRates(provider, id string, fields map[string]json.RawMessage) (config.TokenRates, error) {
	var r config.TokenRates
	var err error
	if r.Input, err = devNumber(fields, "input"); err != nil {
		return r, err
	}
	if r.Output, err = devNumber(fields, "output"); err != nil {
		return r, err
	}
	if r.CacheRead, err = devNumber(fields, "cache_read"); err != nil {
		return r, err
	}
	if provider == "anthropic" {
		write, err := devNumber(fields, "cache_write")
		if err != nil {
			return r, err
		}
		// The feed's generic cache_write currently represents 5m. Only convert
		// when that known relationship holds; a changed relationship needs review.
		if math.Abs(write-r.Input*1.25) > 1e-9 {
			return r, fmt.Errorf("缓存写入价不符合已知 5 分钟口径，需核对 1 小时价格")
		}
		r.CacheWrite = r.Input * 2
	} else if _, ok := fields["cache_write"]; !ok && legacyOpenAI.MatchString(id) {
		// These API model families have no separate cache-write price. This is
		// an explicit compatibility rule, not a general missing-value default.
		r.CacheWrite = 0
	} else if r.CacheWrite, err = devNumber(fields, "cache_write"); err != nil {
		return r, err
	}
	return r, nil
}

func devPrice(provider, id string, model devModel) (config.ModelPrice, error) {
	var p config.ModelPrice
	allowed := map[string]bool{"input": true, "output": true, "cache_read": true, "cache_write": true, "tiers": true, "context_over_200k": true}
	for key := range model.Cost {
		if !allowed[key] {
			return p, fmt.Errorf("未支持的价格字段 %s", key)
		}
	}
	r, err := devRates(provider, id, model.Cost)
	if err != nil {
		return p, err
	}
	p.TokenRates = r
	var tiers []map[string]json.RawMessage
	if raw, ok := model.Cost["tiers"]; ok {
		if string(raw) == "null" {
			return p, fmt.Errorf("长上下文档位为空")
		}
		if err := json.Unmarshal(raw, &tiers); err != nil {
			return p, fmt.Errorf("长上下文档位格式错误")
		}
	}
	if len(tiers) > 1 {
		return p, fmt.Errorf("存在多个价格阶梯，当前仅支持一档长上下文")
	}
	if provider == "anthropic" && len(tiers) > 0 {
		// The current tier selector counts input + cache read (the Codex
		// convention). Claude tiers would also require cache-write tokens in
		// the threshold. Do not import a rule the billing engine cannot express.
		return p, fmt.Errorf("Claude 长上下文阶梯需先适配缓存写入的阈值口径")
	}
	if len(tiers) == 1 {
		tier := tiers[0]
		for key := range tier {
			if key != "tier" && key != "input" && key != "output" && key != "cache_read" && key != "cache_write" {
				return p, fmt.Errorf("未支持的阶梯价格字段 %s", key)
			}
		}
		var threshold struct {
			Type string `json:"type"`
			Size int64  `json:"size"`
		}
		decoder := json.NewDecoder(bytes.NewReader(tier["tier"]))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&threshold); err != nil || threshold.Type != "context" || threshold.Size <= 0 || threshold.Size >= model.Limit.Context {
			return p, fmt.Errorf("长上下文阈值缺失或无效")
		}
		long, err := devRates(provider, id, tier)
		if err != nil {
			return p, fmt.Errorf("长上下文：%w", err)
		}
		p.LongContextOver, p.Long = threshold.Size, &long
	}
	// models.dev retains a legacy field whose name is NOT the true threshold
	// (e.g. GPT-5.5 uses 272000). Never infer 200000 from that field's name.
	if legacy, ok := model.Cost["context_over_200k"]; ok {
		if p.Long == nil {
			return p, fmt.Errorf("仅有旧长上下文字段，缺少明确阈值")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(legacy, &fields); err != nil {
			return p, fmt.Errorf("旧长上下文价格格式错误")
		}
		rates, err := devRates(provider, id, fields)
		if err != nil || rates != *p.Long {
			return p, fmt.Errorf("新旧长上下文价格不一致")
		}
	}
	if model.Limit.Context <= 0 {
		return p, fmt.Errorf("缺少上下文上限，无法核对阶梯价格")
	}
	if p.Long == nil && provider == "anthropic" && model.Limit.Context > 200000 {
		version := claudeVersion.FindStringSubmatch(id)
		major, minor := 0, 0
		if len(version) > 0 {
			major, _ = strconv.Atoi(version[1])
			minor, _ = strconv.Atoi(version[2])
		}
		// Official rule: Claude 4.6+ has no long-context surcharge. Older models
		// with a large window and no tier in the feed must remain unconverted.
		if major < 4 || (major == 4 && minor < 6) {
			return p, fmt.Errorf("旧 Claude 长上下文模型缺少阶梯价格")
		}
	}
	if p.Long == nil && provider == "openai" && model.Limit.Context > 400000 && !strings.HasPrefix(id, "gpt-4.1") {
		return p, fmt.Errorf("大上下文模型缺少明确阶梯价格")
	}
	return p, config.ValidatePrices(map[string]config.ModelPrice{id: p})
}

// ParseModelsDev imports canonical provider IDs only. Unsupported/incomplete
// models become visible issues and can never overwrite an active price.
func ParseModelsDev(raw []byte, fetched time.Time) (Catalog, error) {
	var providers map[string]struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if len(raw) > ModelsDevMaxBytes {
		return Catalog{}, fmt.Errorf("models.dev 数据超过 16 MiB")
	}
	if err := json.Unmarshal(raw, &providers); err != nil {
		return Catalog{}, fmt.Errorf("models.dev 格式错误: %w", err)
	}
	c := Catalog{Schema: 1, Source: "models.dev", PublishedAt: fetched.UTC().Format(time.RFC3339), Entries: map[string]Entry{}}
	for _, provider := range []string{"anthropic", "openai"} {
		data, ok := providers[provider]
		if !ok || len(data.Models) == 0 {
			return c, fmt.Errorf("models.dev 缺少 %s 模型目录", provider)
		}
		for id, rawModel := range data.Models {
			if provider == "anthropic" && !strings.HasPrefix(id, "claude-") {
				continue
			}
			if provider == "openai" && !strings.HasPrefix(id, "gpt-") && !openAIReasoning.MatchString(id) {
				continue
			}
			var model devModel
			err := json.Unmarshal(rawModel, &model)
			if err == nil && (model.Status == "deprecated" || (len(model.Modalities.Output) > 0 && !(len(model.Modalities.Output) == 1 && model.Modalities.Output[0] == "text"))) {
				continue
			}
			reason := ""
			if err != nil {
				reason = "模型价格或元数据格式错误"
			} else if model.ID != "" && model.ID != id {
				reason = "模型 ID 与目录键不一致"
			} else if len(model.Modalities.Output) == 0 {
				reason = "缺少输出类型，无法确认文本计价"
			}
			var price config.ModelPrice
			if reason == "" {
				price, err = devPrice(provider, id, model)
				if err != nil {
					reason = err.Error()
				}
			}
			if reason != "" {
				c.Issues = append(c.Issues, Issue{Model: id, Reason: reason})
				continue
			}
			notes := "models.dev 第三方数据；标准 API 档位，未人工核验。"
			if provider == "anthropic" {
				notes += "缓存写入按 Claude Code 的 1 小时口径（2× 输入价）；未使用源中的 5 分钟价。"
			}
			if provider == "openai" && price.CacheWrite == 0 && legacyOpenAI.MatchString(id) {
				notes += "已知旧版 API 模型不单独收取缓存写入费。"
			}
			c.Entries[id] = Entry{Price: price, SourceURL: ModelsDevURL, Notes: notes}
		}
	}
	sort.Slice(c.Issues, func(i, j int) bool { return c.Issues[i].Model < c.Issues[j].Model })
	// Stable version for equivalent selected prices, independent of retrieval
	// time and unrelated providers. Version the conversion rules as well.
	payload, _ := json.Marshal([]any{c.Entries, c.Issues})
	hash := sha256.Sum256(payload)
	c.Version = "models.dev-v1-" + hex.EncodeToString(hash[:8])
	encoded, _ := json.Marshal(c)
	return Parse(encoded, false)
}
