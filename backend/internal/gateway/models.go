package gateway

import (
	"fmt"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk"
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
	if p, ok := pricingRegistry[modelID]; ok {
		return p
	}
	lower := strings.ToLower(modelID)
	for id, p := range pricingRegistry {
		if strings.HasPrefix(modelID, id) {
			return p
		}
	}
	switch {
	case strings.Contains(lower, "opus"):
		return pricingRegistry["claude-opus-4-7"]
	case strings.Contains(lower, "haiku"):
		return pricingRegistry["claude-haiku-4-5-20251001"]
	case strings.Contains(lower, "sonnet"):
		return pricingRegistry["claude-sonnet-4-6"]
	}
	return fallbackPricing
}

func fillUsageCost(usage *sdk.Usage) {
	if usage == nil || usage.Model == "" {
		return
	}
	p := lookupPricing(usage.Model)
	model := sdk.ModelInfo{
		InputPrice:           p.InputPrice,
		OutputPrice:          p.OutputPrice,
		CachedInputPrice:     p.CachedPrice,
		CacheCreationPrice:   p.CacheCreationPrice,
		CacheCreation1hPrice: p.CacheCreation1hPrice,
	}
	cost := sdk.CalculateCost(sdk.CostInput{
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CachedInputTokens: usage.CachedInputTokens,
	}, model)
	usage.InputCost = cost.InputCost
	usage.OutputCost = cost.OutputCost
	usage.CachedInputCost = cost.CachedInputCost
	usage.CacheCreationCost = cost.CacheCreationCost
	usage.InputPrice = p.InputPrice
	usage.OutputPrice = p.OutputPrice
	usage.CachedInputPrice = p.CachedPrice
	usage.CacheCreationPrice = p.CacheCreationPrice
	usage.CacheCreation1hPrice = p.CacheCreation1hPrice
}

// MapToKiroModel 将 Anthropic 模型名映射到 Kiro 模型 ID 和上下文窗口大小。
func MapToKiroModel(model string) (kiroID string, contextWindow int, err error) {
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
	for _, m := range kiroModels {
		if m.KiroID == kiroID {
			return m.AnthropicID
		}
	}
	return kiroID
}

func allModelInfos() []sdk.ModelInfo {
	out := make([]sdk.ModelInfo, len(kiroModels))
	for i, m := range kiroModels {
		out[i] = sdk.ModelInfo{
			ID:            m.AnthropicID,
			Name:          m.Name,
			ContextWindow: m.ContextWindow,
			MaxOutputTokens: m.MaxOutput,
		}
	}
	return out
}

func buildModelsResponse() []byte {
	var sb strings.Builder
	sb.WriteString(`{"object":"list","data":[`)
	for i, m := range kiroModels {
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
