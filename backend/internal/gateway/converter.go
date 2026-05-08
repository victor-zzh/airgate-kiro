package gateway

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

const (
	maxToolNameLen     = 63
	truncToolNameLen   = 55
	maxThinkingBudget  = 24576
)

// ConvertContext 贯穿请求生命周期的协议转换上下文。
type ConvertContext struct {
	ToolNameMap    map[string]string // 截断名 → 原名
	KiroModelID    string
	AnthropicModel string
	ContextWindow  int
}

type convertConfig struct {
	ProfileArn string
}

// convertRequest 将 Anthropic Messages API 请求体转换为 Kiro ConversationState 格式。
func convertRequest(body []byte, account *sdk.Account, cfg convertConfig) ([]byte, *ConvertContext, error) {
	parsed := gjson.ParseBytes(body)
	model := parsed.Get("model").String()
	kiroID, ctxWin, err := MapToKiroModel(model)
	if err != nil {
		return nil, nil, err
	}

	convCtx := &ConvertContext{
		KiroModelID:    kiroID,
		AnthropicModel: model,
		ContextWindow:  ctxWin,
	}

	conversationID := extractConversationID(parsed)
	systemPrompt := extractSystemPrompt(parsed)
	thinkingPrefix := buildThinkingPrefix(parsed)
	messages := parsed.Get("messages").Array()

	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("messages array is empty")
	}

	// 截断 assistant prefill（最后消息是 assistant 则去掉）
	for len(messages) > 0 && messages[len(messages)-1].Get("role").String() == "assistant" {
		messages = messages[:len(messages)-1]
	}
	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("no user message found after trimming assistant prefill")
	}

	// 转换 tools
	tools, toolNameMap := convertTools(parsed.Get("tools").Array())
	convCtx.ToolNameMap = toolNameMap

	// 构建 history
	history := buildHistory(systemPrompt, thinkingPrefix, messages[:len(messages)-1], kiroID)

	// 清理孤立 tool_use / tool_result
	history = cleanOrphanToolPairs(history)

	// 构建 currentMessage
	lastMsg := messages[len(messages)-1]
	currentMessage := buildCurrentMessage(lastMsg, kiroID, tools)

	// 组装 ConversationState
	state := map[string]any{
		"conversationState": map[string]any{
			"conversationId":      conversationID,
			"agentContinuationId": uuid.New().String(),
			"agentTaskType":       "vibe",
			"chatTriggerType":     "MANUAL",
			"currentMessage":      currentMessage,
			"history":             history,
		},
	}

	if cfg.ProfileArn != "" {
		state["profileArn"] = cfg.ProfileArn
	}

	result, err := json.Marshal(state)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal kiro request: %w", err)
	}

	return result, convCtx, nil
}

var sessionUUIDRegexp = regexp.MustCompile(`session_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

func extractConversationID(parsed gjson.Result) string {
	userID := parsed.Get("metadata.user_id").String()
	if userID != "" {
		// 尝试 JSON 格式
		var obj map[string]string
		if json.Unmarshal([]byte(userID), &obj) == nil {
			if sid, ok := obj["session_id"]; ok && len(sid) == 36 {
				return sid
			}
		}
		// 尝试字符串格式
		if m := sessionUUIDRegexp.FindStringSubmatch(userID); len(m) > 1 {
			return m[1]
		}
	}
	return uuid.New().String()
}

func extractSystemPrompt(parsed gjson.Result) string {
	sys := parsed.Get("system")
	if !sys.Exists() {
		return ""
	}
	if sys.Type == gjson.String {
		return sys.String()
	}
	// 数组格式
	if sys.IsArray() {
		var sb strings.Builder
		for _, block := range sys.Array() {
			if block.Get("type").String() == "text" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(block.Get("text").String())
			}
		}
		return sb.String()
	}
	return ""
}

func buildThinkingPrefix(parsed gjson.Result) string {
	thinking := parsed.Get("thinking")
	if !thinking.Exists() {
		return ""
	}
	thinkType := thinking.Get("type").String()
	switch thinkType {
	case "enabled":
		budget := thinking.Get("budget_tokens").Int()
		if budget > maxThinkingBudget {
			budget = maxThinkingBudget
		}
		if budget <= 0 {
			budget = maxThinkingBudget
		}
		return fmt.Sprintf("<thinking_mode>enabled</thinking_mode><max_thinking_length>%d</max_thinking_length>", budget)
	case "adaptive":
		effort := thinking.Get("thinking_effort").String()
		if effort == "" {
			effort = "medium"
		}
		return fmt.Sprintf("<thinking_mode>adaptive</thinking_mode><thinking_effort>%s</thinking_effort>", effort)
	default:
		return ""
	}
}

func buildHistory(systemPrompt, thinkingPrefix string, messages []gjson.Result, kiroModelID string) []any {
	var history []any

	// System prompt → 首对 user/assistant 消息
	if systemPrompt != "" || thinkingPrefix != "" {
		content := thinkingPrefix
		if systemPrompt != "" {
			if content != "" {
				content += "\n"
			}
			content += systemPrompt
		}
		history = append(history,
			map[string]any{
				"userInputMessage": map[string]any{
					"content": content,
					"modelId": kiroModelID,
					"origin":  "AI_EDITOR",
				},
			},
			map[string]any{
				"assistantResponseMessage": map[string]any{
					"content": "I will follow these instructions.",
				},
			},
		)
	}

	for _, msg := range messages {
		role := msg.Get("role").String()
		switch role {
		case "user":
			history = append(history, buildUserHistoryMessage(msg, kiroModelID))
		case "assistant":
			history = append(history, buildAssistantHistoryMessage(msg))
		}
	}

	return history
}

func buildUserHistoryMessage(msg gjson.Result, kiroModelID string) map[string]any {
	content := msg.Get("content")
	textContent, toolResults, images := extractUserContent(content)

	userMsg := map[string]any{
		"content": textContent,
		"modelId": kiroModelID,
		"origin":  "AI_EDITOR",
	}
	if len(images) > 0 {
		userMsg["images"] = images
	}

	ctx := map[string]any{}
	if len(toolResults) > 0 {
		ctx["toolResults"] = toolResults
	}
	if len(ctx) > 0 {
		userMsg["userInputMessageContext"] = ctx
	}

	return map[string]any{"userInputMessage": userMsg}
}

func extractUserContent(content gjson.Result) (string, []any, []any) {
	if content.Type == gjson.String {
		return content.String(), nil, nil
	}

	var textParts []string
	var toolResults []any
	var images []any

	for _, block := range content.Array() {
		switch block.Get("type").String() {
		case "text":
			textParts = append(textParts, block.Get("text").String())
		case "image":
			src := block.Get("source")
			if src.Exists() {
				images = append(images, map[string]any{
					"format": src.Get("media_type").String(),
					"source": map[string]any{
						"bytes": src.Get("data").String(),
					},
				})
			}
		case "tool_result":
			tr := map[string]any{
				"toolUseId": block.Get("tool_use_id").String(),
				"status":    "success",
			}
			if block.Get("is_error").Bool() {
				tr["status"] = "error"
			}
			resultContent := block.Get("content")
			if resultContent.Type == gjson.String {
				tr["content"] = []any{map[string]string{"text": resultContent.String()}}
			} else if resultContent.IsArray() {
				var parts []any
				for _, c := range resultContent.Array() {
					if c.Get("type").String() == "text" {
						parts = append(parts, map[string]string{"text": c.Get("text").String()})
					}
				}
				if len(parts) > 0 {
					tr["content"] = parts
				}
			}
			toolResults = append(toolResults, tr)
		}
	}

	return strings.Join(textParts, "\n"), toolResults, images
}

func buildAssistantHistoryMessage(msg gjson.Result) map[string]any {
	content := msg.Get("content")

	var textParts []string
	var toolUses []any

	if content.Type == gjson.String {
		textParts = append(textParts, content.String())
	} else if content.IsArray() {
		for _, block := range content.Array() {
			switch block.Get("type").String() {
			case "text":
				textParts = append(textParts, block.Get("text").String())
			case "tool_use":
				inputRaw := block.Get("input").Raw
				if inputRaw == "" {
					inputRaw = "{}"
				}
				toolUses = append(toolUses, map[string]any{
					"toolUseId": block.Get("id").String(),
					"name":      block.Get("name").String(),
					"input":     json.RawMessage(inputRaw),
				})
			}
		}
	}

	assistant := map[string]any{
		"content": strings.Join(textParts, "\n"),
	}
	if len(toolUses) > 0 {
		assistant["toolUses"] = toolUses
	}

	return map[string]any{"assistantResponseMessage": assistant}
}

func buildCurrentMessage(msg gjson.Result, kiroModelID string, tools []any) map[string]any {
	content := msg.Get("content")
	textContent, toolResults, images := extractUserContent(content)

	userMsg := map[string]any{
		"content": textContent,
		"modelId": kiroModelID,
		"origin":  "AI_EDITOR",
		"images":  orEmptySlice(images),
	}

	msgCtx := map[string]any{}
	if len(tools) > 0 {
		msgCtx["tools"] = tools
	}
	if len(toolResults) > 0 {
		msgCtx["toolResults"] = toolResults
	}
	if len(msgCtx) > 0 {
		userMsg["userInputMessageContext"] = msgCtx
	}

	return map[string]any{"userInputMessage": userMsg}
}

func convertTools(tools []gjson.Result) ([]any, map[string]string) {
	if len(tools) == 0 {
		return nil, nil
	}

	nameMap := make(map[string]string)
	var result []any

	for _, tool := range tools {
		name := tool.Get("name").String()
		desc := tool.Get("description").String()
		schema := tool.Get("input_schema").Raw
		if schema == "" {
			schema = `{"type":"object","properties":{},"required":[]}`
		}

		// 规范化 schema
		schema = normalizeSchema(schema)

		actualName := name
		if len(name) > maxToolNameLen {
			hash := sha256Hex(name)
			actualName = name[:truncToolNameLen] + "_" + hash[:7]
			nameMap[actualName] = name
		}

		result = append(result, map[string]any{
			"toolSpecification": map[string]any{
				"name":        actualName,
				"description": desc,
				"inputSchema": map[string]any{
					"json": json.RawMessage(schema),
				},
			},
		})
	}

	return result, nameMap
}

func normalizeSchema(schema string) string {
	var s string
	s = schema

	parsed := gjson.Parse(s)
	if !parsed.Get("type").Exists() || parsed.Get("type").String() == "" {
		s, _ = sjson.Set(s, "type", "object")
	}
	if !parsed.Get("properties").Exists() || parsed.Get("properties").Type == gjson.Null {
		s, _ = sjson.Set(s, "properties", map[string]any{})
	}
	if parsed.Get("required").Type == gjson.Null {
		s, _ = sjson.Set(s, "required", []any{})
	}

	return s
}

func cleanOrphanToolPairs(history []any) []any {
	toolUseIDs := make(map[string]bool)
	toolResultIDs := make(map[string]bool)

	// 第一遍：收集所有 tool_use 和 tool_result 的 ID
	for _, entry := range history {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if assistant, ok := m["assistantResponseMessage"].(map[string]any); ok {
			if uses, ok := assistant["toolUses"].([]any); ok {
				for _, u := range uses {
					if um, ok := u.(map[string]any); ok {
						if id, ok := um["toolUseId"].(string); ok {
							toolUseIDs[id] = true
						}
					}
				}
			}
		}
		if user, ok := m["userInputMessage"].(map[string]any); ok {
			if ctx, ok := user["userInputMessageContext"].(map[string]any); ok {
				if results, ok := ctx["toolResults"].([]any); ok {
					for _, r := range results {
						if rm, ok := r.(map[string]any); ok {
							if id, ok := rm["toolUseId"].(string); ok {
								toolResultIDs[id] = true
							}
						}
					}
				}
			}
		}
	}

	// 第二遍：清理孤立的
	var cleaned []any
	for _, entry := range history {
		m, ok := entry.(map[string]any)
		if !ok {
			cleaned = append(cleaned, entry)
			continue
		}

		if assistant, ok := m["assistantResponseMessage"].(map[string]any); ok {
			if uses, ok := assistant["toolUses"].([]any); ok {
				var validUses []any
				for _, u := range uses {
					if um, ok := u.(map[string]any); ok {
						if id, ok := um["toolUseId"].(string); ok && toolResultIDs[id] {
							validUses = append(validUses, u)
						}
					}
				}
				if len(validUses) > 0 {
					assistant["toolUses"] = validUses
				} else {
					delete(assistant, "toolUses")
				}
			}
		}

		if user, ok := m["userInputMessage"].(map[string]any); ok {
			if ctx, ok := user["userInputMessageContext"].(map[string]any); ok {
				if results, ok := ctx["toolResults"].([]any); ok {
					var validResults []any
					for _, r := range results {
						if rm, ok := r.(map[string]any); ok {
							if id, ok := rm["toolUseId"].(string); ok && toolUseIDs[id] {
								validResults = append(validResults, r)
							}
						}
					}
					if len(validResults) > 0 {
						ctx["toolResults"] = validResults
					} else {
						delete(ctx, "toolResults")
					}
				}
				if len(ctx) == 0 {
					delete(user, "userInputMessageContext")
				}
			}
		}

		cleaned = append(cleaned, entry)
	}

	return cleaned
}

func orEmptySlice(s []any) []any {
	if s == nil {
		return []any{}
	}
	return s
}
