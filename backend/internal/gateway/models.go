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
	{KiroID: "claude-haiku-4.5", AnthropicID: "claude-haiku-4-5-20251001", Name: "Claude Haiku 4.5", ContextWindow: 200_000, MaxOutput: 64_000},
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
