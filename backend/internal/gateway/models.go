package gateway

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

type kiroModelSpec struct {
	KiroID        string
	AnthropicID   string
	Name          string
	ContextWindow int
	MaxOutput     int
}

var kiroModels = []kiroModelSpec{
	{KiroID: "claude-sonnet-4.5", AnthropicID: "claude-sonnet-4-5-20250929", Name: "Claude Sonnet 4.5", ContextWindow: 200_000, MaxOutput: 64_000},
	{KiroID: "claude-sonnet-4.6", AnthropicID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", ContextWindow: 1_000_000, MaxOutput: 64_000},
	{KiroID: "claude-opus-4.5", AnthropicID: "claude-opus-4-5-20251101", Name: "Claude Opus 4.5", ContextWindow: 200_000, MaxOutput: 64_000},
	{KiroID: "claude-opus-4.6", AnthropicID: "claude-opus-4-6", Name: "Claude Opus 4.6", ContextWindow: 1_000_000, MaxOutput: 128_000},
	{KiroID: "claude-opus-4.7", AnthropicID: "claude-opus-4-7", Name: "Claude Opus 4.7", ContextWindow: 1_000_000, MaxOutput: 128_000},
	{KiroID: "claude-haiku-4.5", AnthropicID: "claude-haiku-4-5-20251001", Name: "Claude Haiku 4.5", ContextWindow: 200_000, MaxOutput: 64_000},
}

// ── 模型定价（对齐 Anthropic 官方 2026-04 定价，$/1M tokens）──

type pricingSpec struct {
	InputPrice           float64
	CachedPrice          float64
	CacheCreationPrice   float64
	CacheCreation1hPrice float64
	OutputPrice          float64
}

var pricingRegistry = map[string]pricingSpec{
	// Opus — input $5 / cache_read $0.50 / write_5m $6.25 / write_1h $10 / output $25
	"claude-opus-4-7":          {5.0, 0.5, 6.25, 10.0, 25.0},
	"claude-opus-4-6":          {5.0, 0.5, 6.25, 10.0, 25.0},
	"claude-opus-4-5-20251101": {5.0, 0.5, 6.25, 10.0, 25.0},
	// Sonnet — input $3 / cache_read $0.30 / write_5m $3.75 / write_1h $6 / output $15
	"claude-sonnet-4-6":          {3.0, 0.3, 3.75, 6.0, 15.0},
	"claude-sonnet-4-5-20250929": {3.0, 0.3, 3.75, 6.0, 15.0},
	// Haiku — input $1 / cache_read $0.10 / write_5m $1.25 / write_1h $2 / output $5
	"claude-haiku-4-5-20251001": {1.0, 0.1, 1.25, 2.0, 5.0},
}

var fallbackPricing = pricingSpec{3.0, 0.3, 3.75, 6.0, 15.0} // Sonnet 4.6

func lookupPricing(modelID string) pricingSpec {
	reg := activePricingRegistry()
	if p, ok := reg[modelID]; ok {
		return p
	}
	lower := strings.ToLower(modelID)
	for id, p := range reg {
		if strings.HasPrefix(modelID, id) {
			return p
		}
	}
	switch {
	case strings.Contains(lower, "opus"):
		if p, ok := reg["claude-opus-4-7"]; ok {
			warnPricingFallbackOnce(modelID, "claude-opus-4-7")
			return p
		}
	case strings.Contains(lower, "haiku"):
		if p, ok := reg["claude-haiku-4-5-20251001"]; ok {
			warnPricingFallbackOnce(modelID, "claude-haiku-4-5-20251001")
			return p
		}
	case strings.Contains(lower, "sonnet"):
		if p, ok := reg["claude-sonnet-4-6"]; ok {
			warnPricingFallbackOnce(modelID, "claude-sonnet-4-6")
			return p
		}
	}
	warnPricingFallbackOnce(modelID, "sonnet-4-6-fallback")
	return fallbackPricing
}

// 兜底计费告警去重表。上限防被垃圾模型名撑爆内存;到达上限后不再新增告警(已告警的仍去重)。
const pricingFallbackWarnCap = 512

var (
	pricingFallbackWarnMu sync.Mutex
	pricingFallbackWarned = map[string]struct{}{}
)

// warnPricingFallbackOnce 未注册模型按推断/兜底价计费时告警一次(按模型去重)。
// 精确/前缀匹配是同模型的日期变体不告警;关键词推断和 Sonnet 兜底才是"猜价",
// 看到这条日志就该去后台「模型目录」给该模型配置官方价。
func warnPricingFallbackOnce(modelID, billedAs string) {
	pricingFallbackWarnMu.Lock()
	_, seen := pricingFallbackWarned[modelID]
	full := len(pricingFallbackWarned) >= pricingFallbackWarnCap
	if !seen && !full {
		pricingFallbackWarned[modelID] = struct{}{}
	}
	pricingFallbackWarnMu.Unlock()
	if seen || full {
		return
	}
	slog.Warn("model_pricing_fallback",
		"model", modelID,
		"billed_as", billedAs,
		"hint", "未注册模型正按推断价计费,请到后台「模型目录」为其配置官方价",
	)
}

func fillUsageCost(usage *sdk.Usage) {
	if usage == nil || usage.Model == "" {
		return
	}
	p := lookupPricing(usage.Model)
	inputTokens := usageMetricInt(usage, usageMetricInputTokens)
	outputTokens := usageMetricInt(usage, usageMetricOutputTokens)
	cachedInputTokens := usageMetricInt(usage, usageMetricCachedInputTokens)

	inputCost := tokenCost(inputTokens, p.InputPrice)
	cachedCost := tokenCost(cachedInputTokens, p.CachedPrice)
	outputCost := tokenCost(outputTokens, p.OutputPrice)

	setUsageMetric(usage, sdk.UsageMetric{
		Key:         usageMetricInputTokens,
		Label:       "输入 Token",
		Kind:        "token",
		Unit:        "token",
		Value:       float64(inputTokens),
		AccountCost: inputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(p.InputPrice),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:         usageMetricCachedInputTokens,
		Label:       "缓存输入 Token",
		Kind:        "token",
		Unit:        "token",
		Value:       float64(cachedInputTokens),
		AccountCost: cachedCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(p.CachedPrice),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:         usageMetricOutputTokens,
		Label:       "输出 Token",
		Kind:        "token",
		Unit:        "token",
		Value:       float64(outputTokens),
		AccountCost: outputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(p.OutputPrice),
	})
	setUsageCostDetail(usage, sdk.UsageCostDetail{
		Key:         usageCostInput,
		Label:       "输入 Token",
		AccountCost: inputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(p.InputPrice),
	})
	setUsageCostDetail(usage, sdk.UsageCostDetail{
		Key:         usageCostCachedInput,
		Label:       "缓存输入 Token",
		AccountCost: cachedCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(p.CachedPrice),
	})
	setUsageCostDetail(usage, sdk.UsageCostDetail{
		Key:         usageCostOutput,
		Label:       "输出 Token",
		AccountCost: outputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    priceMetadata(p.OutputPrice),
	})
}

func tokenCost(tokens int, pricePerMillion float64) float64 {
	if tokens <= 0 || pricePerMillion <= 0 {
		return 0
	}
	return float64(tokens) * pricePerMillion / 1_000_000
}

func priceMetadata(price float64) map[string]string {
	return map[string]string{
		"unit_price": fmt.Sprintf("%.10g", price),
		"unit":       "USD/1M tokens",
	}
}

// MapToKiroModel 将 Anthropic 模型名映射到 Kiro 模型 ID 和上下文窗口大小。
func MapToKiroModel(model string) (kiroID string, contextWindow int, err error) {
	trimmed := strings.TrimSpace(model)
	for _, m := range activeKiroModels() {
		if strings.EqualFold(trimmed, m.AnthropicID) || strings.EqualFold(trimmed, m.KiroID) {
			return m.KiroID, m.ContextWindow, nil
		}
	}

	lower := strings.ToLower(model)

	if strings.Contains(lower, "sonnet") {
		if strings.Contains(lower, "4-6") || strings.Contains(lower, "4.6") {
			return "claude-sonnet-4.6", 1_000_000, nil
		}
		return "claude-sonnet-4.5", 200_000, nil
	}
	if strings.Contains(lower, "opus") {
		if strings.Contains(lower, "4-5") || strings.Contains(lower, "4.5") {
			return "claude-opus-4.5", 200_000, nil
		}
		if strings.Contains(lower, "4-7") || strings.Contains(lower, "4.7") {
			return "claude-opus-4.7", 1_000_000, nil
		}
		return "claude-opus-4.6", 1_000_000, nil
	}
	if strings.Contains(lower, "haiku") {
		return "claude-haiku-4.5", 200_000, nil
	}
	return "", 0, fmt.Errorf("unsupported model: %s", model)
}

// MapToAnthropicModel 将 Kiro 模型 ID 反向映射为 Anthropic 模型名。
func MapToAnthropicModel(kiroID string) string {
	for _, m := range activeKiroModels() {
		if m.KiroID == kiroID {
			return m.AnthropicID
		}
	}
	return kiroID
}

func allModelInfos() []sdk.ModelInfo {
	models := visibleKiroModels()
	out := make([]sdk.ModelInfo, len(models))
	for i, m := range models {
		out[i] = sdk.ModelInfo{
			ID:              m.AnthropicID,
			Name:            m.Name,
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxOutput,
			Capabilities:    []string{sdk.ModelCapChat, sdk.ModelCapReasoning},
		}
	}
	return out
}

func buildModelsResponse() []byte {
	var sb strings.Builder
	sb.WriteString(`{"object":"list","data":[`)
	for i, m := range visibleKiroModels() {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb,
			`{"id":%q,"object":"model","display_name":%q,"created_at":"2025-01-01T00:00:00Z","type":"model"}`,
			m.AnthropicID, m.Name,
		)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}
