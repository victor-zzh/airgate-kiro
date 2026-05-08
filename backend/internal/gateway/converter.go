package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

const (
	maxToolNameLen    = 63
	truncToolNameLen  = 55
	maxThinkingBudget = 102400
)

// ConvertContext 贯穿请求生命周期的协议转换上下文。
type ConvertContext struct {
	ToolNameMap    map[string]string // 截断名 → 原名
	KiroModelID    string
	AnthropicModel string
	ContextWindow  int
	SystemPrompt   string // 用于 cache simulation
	ToolsJSON      string // 用于 cache simulation
}

type convertConfig struct {
	ProfileArn string
}

// convertRequest 将 Anthropic Messages API 请求体转换为 Kiro ConversationState 格式。
func convertRequest(body []byte, account *sdk.Account, cfg convertConfig, logger *slog.Logger) ([]byte, *ConvertContext, error) {
	parsed := gjson.ParseBytes(body)
	model := parsed.Get("model").String()
	kiroID, ctxWin, err := MapToKiroModel(model)
	if err != nil {
		return nil, nil, err
	}

	conversationID := extractConversationID(parsed)
	systemPrompt := extractSystemPrompt(parsed)
	thinkingPrefix := buildThinkingPrefix(parsed)

	convCtx := &ConvertContext{
		KiroModelID:    kiroID,
		AnthropicModel: model,
		ContextWindow:  ctxWin,
		SystemPrompt:   systemPrompt,
	}
	messages := parsed.Get("messages").Array()

	logger.Debug("convert_input",
		"model", model,
		"kiro_model", kiroID,
		"system_prompt_len", len(systemPrompt),
		"thinking_prefix", thinkingPrefix,
		"messages_count", len(messages),
		"tools_count", len(parsed.Get("tools").Array()),
		"has_profile_arn", cfg.ProfileArn != "",
		"conversation_id", conversationID,
	)

	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("messages array is empty")
	}

	// 统计消息角色分布
	roleCounts := map[string]int{}
	for _, msg := range messages {
		roleCounts[msg.Get("role").String()]++
	}
	logger.Debug("convert_messages_breakdown", "role_counts", fmt.Sprintf("%v", roleCounts))

	// 截断 assistant prefill（最后消息是 assistant 则去掉）
	trimmed := 0
	for len(messages) > 0 && messages[len(messages)-1].Get("role").String() == "assistant" {
		messages = messages[:len(messages)-1]
		trimmed++
	}
	if trimmed > 0 {
		logger.Debug("convert_trimmed_assistant_prefill", "trimmed_count", trimmed)
	}
	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("no user message found after trimming assistant prefill")
	}

	// 转换 tools
	tools, toolNameMap := convertTools(parsed.Get("tools").Array())
	convCtx.ToolNameMap = toolNameMap
	convCtx.ToolsJSON = parsed.Get("tools").Raw
	if len(toolNameMap) > 0 {
		logger.Debug("convert_tool_names_truncated", "truncated_count", len(toolNameMap))
	}

	// 记录工具详细信息
	if len(tools) > 0 {
		var toolNames []string
		var maxSchemaSize int
		for _, t := range tools {
			spec := t.(map[string]any)["toolSpecification"].(map[string]any)
			toolNames = append(toolNames, spec["name"].(string))
			if schema, ok := spec["inputSchema"]; ok {
				schemaBytes, _ := json.Marshal(schema)
				if len(schemaBytes) > maxSchemaSize {
					maxSchemaSize = len(schemaBytes)
				}
			}
		}
		toolsJSON, _ := json.Marshal(tools)
		logger.Debug("convert_tools",
			"tool_count", len(tools),
			"tools_total_bytes", len(toolsJSON),
			"max_schema_bytes", maxSchemaSize,
			"tool_names", strings.Join(toolNames, ", "),
		)
	}

	// 构建 history
	history := buildHistory(systemPrompt, thinkingPrefix, messages[:len(messages)-1], kiroID)
	historyBeforeClean := len(history)

	// 提取 currentMessage 中的 tool_result ID，避免清理掉 history 中对应的 tool_use
	lastMsg := messages[len(messages)-1]
	currentToolResultIDs := extractToolResultIDs(lastMsg)

	// 清理孤立 tool_use / tool_result
	history = cleanOrphanToolPairs(history, currentToolResultIDs)

	historyJSON, _ := json.Marshal(history)
	logger.Debug("convert_history",
		"history_count_before_clean", historyBeforeClean,
		"history_count_after_clean", len(history),
		"history_total_bytes", len(historyJSON),
		"current_msg_tool_result_ids", len(currentToolResultIDs),
	)
	currentMessage := buildCurrentMessage(lastMsg, kiroID, tools)
	currentMsgJSON, _ := json.Marshal(currentMessage)

	// 分析 currentMessage 内容
	lastMsgContent := lastMsg.Get("content")
	var contentTypes []string
	if lastMsgContent.Type == gjson.String {
		contentTypes = append(contentTypes, fmt.Sprintf("text(%d chars)", len(lastMsgContent.String())))
	} else if lastMsgContent.IsArray() {
		for _, block := range lastMsgContent.Array() {
			bt := block.Get("type").String()
			switch bt {
			case "text":
				contentTypes = append(contentTypes, fmt.Sprintf("text(%d chars)", len(block.Get("text").String())))
			case "tool_result":
				trContent := block.Get("content").String()
				contentTypes = append(contentTypes, fmt.Sprintf("tool_result(id=%s, %d chars)", block.Get("tool_use_id").String()[:8], len(trContent)))
			case "image":
				contentTypes = append(contentTypes, "image")
			default:
				contentTypes = append(contentTypes, bt)
			}
		}
	}
	logger.Debug("convert_current_message",
		"last_msg_role", lastMsg.Get("role").String(),
		"current_message_bytes", len(currentMsgJSON),
		"content_blocks", strings.Join(contentTypes, "; "),
	)

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

	logger.Debug("convert_result",
		"total_bytes", len(result),
		"total_mb", fmt.Sprintf("%.2f", float64(len(result))/1024/1024),
	)

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
				text := block.Get("text").String()
				// 过滤 Claude Code 注入的 billing header
				if strings.HasPrefix(text, "x-anthropic-billing-header: ") {
					continue
				}
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(text)
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

	if textContent == "" && len(toolResults) > 0 {
		textContent = "Here are the tool results."
	}

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
			if img := convertImageBlock(block); img != nil {
				images = append(images, img)
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
					switch c.Get("type").String() {
					case "text":
						parts = append(parts, map[string]string{"text": c.Get("text").String()})
					case "image":
						// Claude Code 截图/读图结果包含 image 块
						if img := convertImageBlock(c); img != nil {
							images = append(images, img)
						}
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

func convertImageBlock(block gjson.Result) map[string]any {
	src := block.Get("source")
	if !src.Exists() {
		return nil
	}
	data := src.Get("data").String()
	if data == "" {
		data = src.Get("base64").String()
	}
	if data == "" {
		return nil
	}
	mediaType := src.Get("media_type").String()
	if mediaType == "" {
		mediaType = src.Get("mime_type").String()
	}
	if mediaType == "" {
		mediaType = "image/png"
	}
	// Kiro 要求短格式名（"png"），不接受完整 MIME 类型（"image/png"）
	format := mediaType
	if idx := strings.IndexByte(format, '/'); idx >= 0 && idx < len(format)-1 {
		format = format[idx+1:]
	}
	return map[string]any{
		"format": format,
		"source": map[string]any{
			"bytes": data,
		},
	}
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
			case "thinking", "redacted_thinking":
				// Anthropic extended thinking 产物，Kiro 无此概念，显式跳过
				continue
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

	// Kiro 要求 content 非空；当消息只有 tool_result 没有文本时填充占位符
	if textContent == "" && len(toolResults) > 0 {
		textContent = "Here are the tool results."
	}

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
		if isServerTool(tool) {
			continue
		}

		name := tool.Get("name").String()
		desc := tool.Get("description").String()
		schema := tool.Get("input_schema").Raw
		if schema == "" {
			schema = `{"type":"object","properties":{},"required":[]}`
		}

		// 规范化 schema
		schema = normalizeSchema(schema)

		actualName := shortenToolName(name)
		if actualName != name {
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

// shortenToolName 缩短工具名到 maxToolNameLen，MCP 工具优先保留工具名部分。
func shortenToolName(name string) string {
	if len(name) <= maxToolNameLen {
		return name
	}
	// MCP 工具格式: mcp__server_name__tool_name
	// 优先保留 mcp__ 前缀和最后一个 __ 后的工具名
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 4 { // 排除 mcp__ 本身的 __
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) <= maxToolNameLen {
				return candidate
			}
		}
	}
	// 回退：截断 + hash
	hash := sha256Hex(name)
	return name[:truncToolNameLen] + "_" + hash[:7]
}

func normalizeSchema(schema string) string {
	s := schema

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
	// 剥离 JSON Schema 元字段，避免上游校验失败
	if parsed.Get("\\$schema").Exists() {
		s, _ = sjson.Delete(s, "\\$schema")
	}

	return s
}

// extractToolResultIDs 从 Anthropic 格式的用户消息中提取所有 tool_result 的 tool_use_id。
func extractToolResultIDs(msg gjson.Result) map[string]bool {
	ids := make(map[string]bool)
	content := msg.Get("content")
	if !content.IsArray() {
		return ids
	}
	for _, block := range content.Array() {
		if block.Get("type").String() == "tool_result" {
			if id := block.Get("tool_use_id").String(); id != "" {
				ids[id] = true
			}
		}
	}
	return ids
}

// cleanOrphanToolPairs 清理 history 中孤立的 tool_use / tool_result。
// currentMsgResultIDs 是 currentMessage 中已有的 tool_result ID，
// 对应的 tool_use 在 history 中不应被清除。
func cleanOrphanToolPairs(history []any, currentMsgResultIDs map[string]bool) []any {
	toolUseIDs := make(map[string]bool)
	toolResultIDs := make(map[string]bool)

	// currentMessage 中的 tool_result ID 也算有效配对
	for id := range currentMsgResultIDs {
		toolResultIDs[id] = true
	}

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
