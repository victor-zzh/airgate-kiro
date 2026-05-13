package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
	"github.com/google/uuid"
)

// SSEConverter 将 Kiro EventStream 事件转换为 Anthropic SSE 格式。
type SSEConverter struct {
	writer     http.ResponseWriter
	flusher    http.Flusher
	convertCtx *ConvertContext

	messageID  string
	blockIndex int
	inThinking bool
	inText     bool
	inToolUse  bool
	started    bool

	thinkingBuf string

	currentToolName string
	currentToolID   string

	usage *sdk.Usage

	firstTokenOnce bool
	startTime      time.Time
}

func newSSEConverter(w http.ResponseWriter, convertCtx *ConvertContext, start time.Time) *SSEConverter {
	flusher, _ := w.(http.Flusher)
	return &SSEConverter{
		writer:     w,
		flusher:    flusher,
		convertCtx: convertCtx,
		messageID:  "msg_" + uuid.New().String()[:24],
		usage:      newTokenUsage(convertCtx.AnthropicModel, 0, 0, 0, 0),
		startTime:  start,
	}
}

// streamKiroToSSE 读取 Kiro 二进制 Event Stream 并输出 Anthropic SSE。
func streamKiroToSSE(ctx context.Context, body io.Reader, w http.ResponseWriter, convertCtx *ConvertContext, start time.Time) sdk.ForwardOutcome {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	conv := newSSEConverter(w, convertCtx, start)
	decoder := NewEventStreamDecoder(body)

	stopReason := "end_turn"

	for {
		if ctx.Err() != nil {
			return streamAbortedOutcome(http.StatusOK, "context cancelled", conv.usage)
		}

		event, err := decoder.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return streamAbortedOutcome(http.StatusOK, fmt.Sprintf("decode error: %v", err), conv.usage)
		}

		switch event.MessageType {
		case "event":
			switch event.EventType {
			case "assistantResponseEvent":
				conv.handleAssistantResponse(event.Payload)
			case "toolUseEvent":
				stopReason = "tool_use"
				conv.handleToolUse(event.Payload)
			case "contextUsageEvent":
				conv.handleContextUsage(event.Payload)
			case "meteringEvent":
				// 忽略
			}
		case "error", "exception":
			if conv.started {
				return streamAbortedOutcome(http.StatusOK,
					fmt.Sprintf("upstream error: %s %s", event.ErrorCode, event.ErrorMsg), conv.usage)
			}
			return transientOutcome(fmt.Sprintf("upstream error: %s %s", event.ErrorCode, event.ErrorMsg))
		}
	}

	// 关闭最后一个 content block
	conv.closeCurrentBlock()

	// message_delta
	conv.emitSSE("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":"%s"},"usage":{"output_tokens":%d}}`,
		stopReason, usageMetricInt(conv.usage, usageMetricOutputTokens),
	))

	// message_stop
	conv.emitSSE("message_stop", `{"type":"message_stop"}`)

	applyCacheToUsage(conv.usage, convertCtx)
	fillUsageCost(conv.usage)

	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK},
		Usage:    conv.usage,
	}
}

func (c *SSEConverter) handleAssistantResponse(payload []byte) {
	content := ParseAssistantResponsePayload(payload)
	if content == "" {
		return
	}

	if !c.started {
		c.emitMessageStart()
	}

	c.recordFirstToken()

	// Thinking 标签检测
	c.processTextWithThinking(content)
}

func (c *SSEConverter) processTextWithThinking(text string) {
	remaining := c.thinkingBuf + text
	c.thinkingBuf = ""

	for len(remaining) > 0 {
		if c.inThinking {
			endIdx := findThinkingEndTag(remaining)
			if endIdx >= 0 {
				thinkingContent := remaining[:endIdx]
				if thinkingContent != "" {
					c.emitThinkingDelta(thinkingContent)
				}
				c.closeThinkingBlock()
				// 跳过 </thinking> 标签本身和后续的 \n\n
				skip := endIdx + len("</thinking>")
				if skip < len(remaining) && remaining[skip:] == "\n\n" || (skip+2 <= len(remaining) && remaining[skip:skip+2] == "\n\n") {
					skip += 2
				}
				remaining = remaining[skip:]
				continue
			}

			// 可能标签跨 chunk 边界，保留末尾
			if len(remaining) > len("</thinking>")+2 {
				safe := runeAlignBack(remaining, len(remaining)-len("</thinking>")-2)
				if safe > 0 {
					c.emitThinkingDelta(remaining[:safe])
					c.thinkingBuf = remaining[safe:]
				} else {
					c.thinkingBuf = remaining
				}
			} else {
				c.thinkingBuf = remaining
			}
			return
		}

		// 不在 thinking 中
		startIdx := findThinkingStartTag(remaining)
		if startIdx >= 0 {
			// 先输出 thinking 前的文本
			before := remaining[:startIdx]
			if before != "" {
				c.ensureTextBlock()
				c.emitTextDelta(before)
			}
			// 开始 thinking 块
			c.closeCurrentBlock()
			c.openThinkingBlock()
			remaining = remaining[startIdx+len("<thinking>"):]
			// 剥离前导 \n
			remaining = strings.TrimLeft(remaining, "\n")
			continue
		}

		// 没有 thinking 标签
		if len(remaining) > len("<thinking>") {
			safe := runeAlignBack(remaining, len(remaining)-len("<thinking>"))
			if safe > 0 {
				c.ensureTextBlock()
				c.emitTextDelta(remaining[:safe])
				c.thinkingBuf = remaining[safe:]
			} else {
				c.thinkingBuf = remaining
			}
		} else {
			c.thinkingBuf = remaining
		}
		return
	}
}

func (c *SSEConverter) handleToolUse(payload []byte) {
	tu, err := ParseToolUsePayload(payload)
	if err != nil {
		return
	}

	if !c.started {
		c.emitMessageStart()
	}

	c.recordFirstToken()

	// 恢复原始工具名
	name := tu.Name
	if original, ok := c.convertCtx.ToolNameMap[name]; ok {
		name = original
	}

	// 新 tool_use 块
	if tu.ToolUseID != "" && tu.ToolUseID != c.currentToolID {
		c.closeCurrentBlock()
		c.currentToolID = tu.ToolUseID
		c.currentToolName = name

		c.emitSSE("content_block_start", fmt.Sprintf(
			`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"%s","name":"%s","input":{}}}`,
			c.blockIndex, tu.ToolUseID, name,
		))
		c.inToolUse = true
	}

	// 增量 input
	if tu.Input != "" {
		c.emitSSE("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`,
			c.blockIndex, jsonString(tu.Input),
		))
		addUsageOutputTokens(c.usage, estimateTokens(tu.Input))
	}

	// 工具调用结束
	if tu.Stop {
		c.emitSSE("content_block_stop", fmt.Sprintf(
			`{"type":"content_block_stop","index":%d}`, c.blockIndex,
		))
		c.blockIndex++
		c.inToolUse = false
		c.currentToolID = ""
	}
}

func (c *SSEConverter) handleContextUsage(payload []byte) {
	cu, err := ParseContextUsagePayload(payload)
	if err != nil {
		return
	}
	setUsageInputTokens(c.usage, int(cu.ContextUsagePercentage*float64(c.convertCtx.ContextWindow)/100.0))
}

func (c *SSEConverter) emitMessageStart() {
	c.started = true
	c.emitSSE("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","model":"%s","content":[],"stop_reason":null,"usage":{"input_tokens":%d,"output_tokens":0}}}`,
		c.messageID, c.convertCtx.AnthropicModel, usageMetricInt(c.usage, usageMetricInputTokens),
	))
}

func (c *SSEConverter) ensureTextBlock() {
	if c.inText {
		return
	}
	c.closeCurrentBlock()
	c.emitSSE("content_block_start", fmt.Sprintf(
		`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`,
		c.blockIndex,
	))
	c.inText = true
}

func (c *SSEConverter) openThinkingBlock() {
	c.emitSSE("content_block_start", fmt.Sprintf(
		`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`,
		c.blockIndex,
	))
	c.inThinking = true
}

func (c *SSEConverter) closeThinkingBlock() {
	if !c.inThinking {
		return
	}
	c.emitSSE("content_block_stop", fmt.Sprintf(
		`{"type":"content_block_stop","index":%d}`, c.blockIndex,
	))
	c.blockIndex++
	c.inThinking = false
}

func (c *SSEConverter) closeCurrentBlock() {
	if c.inThinking {
		// 刷出缓冲
		if c.thinkingBuf != "" {
			c.emitThinkingDelta(c.thinkingBuf)
			c.thinkingBuf = ""
		}
		c.closeThinkingBlock()
	}
	if c.inText {
		if c.thinkingBuf != "" {
			c.emitTextDelta(c.thinkingBuf)
			c.thinkingBuf = ""
		}
		c.emitSSE("content_block_stop", fmt.Sprintf(
			`{"type":"content_block_stop","index":%d}`, c.blockIndex,
		))
		c.blockIndex++
		c.inText = false
	}
	if c.inToolUse {
		c.emitSSE("content_block_stop", fmt.Sprintf(
			`{"type":"content_block_stop","index":%d}`, c.blockIndex,
		))
		c.blockIndex++
		c.inToolUse = false
		c.currentToolID = ""
	}
}

func (c *SSEConverter) emitTextDelta(text string) {
	c.emitSSE("content_block_delta", fmt.Sprintf(
		`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`,
		c.blockIndex, jsonString(text),
	))
	addUsageOutputTokens(c.usage, estimateTokens(text))
}

func (c *SSEConverter) emitThinkingDelta(text string) {
	c.emitSSE("content_block_delta", fmt.Sprintf(
		`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%s}}`,
		c.blockIndex, jsonString(text),
	))
}

func (c *SSEConverter) emitSSE(event, data string) {
	fmt.Fprintf(c.writer, "event: %s\ndata: %s\n\n", event, data)
	if c.flusher != nil {
		c.flusher.Flush()
	}
}

func (c *SSEConverter) recordFirstToken() {
	if !c.firstTokenOnce {
		c.firstTokenOnce = true
		c.usage.FirstTokenMs = time.Since(c.startTime).Milliseconds()
	}
}

// bufferKiroResponse 非流式模式：收集所有事件后构建完整 Anthropic JSON 响应。
func bufferKiroResponse(ctx context.Context, body io.Reader, w http.ResponseWriter, convertCtx *ConvertContext, start time.Time) sdk.ForwardOutcome {
	decoder := NewEventStreamDecoder(body)

	var contentBlocks []any
	var inputTokens int
	outputTokens := 0
	stopReason := "end_turn"

	for {
		event, err := decoder.Next()
		if err != nil {
			break
		}

		if event.MessageType != "event" {
			if event.MessageType == "error" || event.MessageType == "exception" {
				return transientOutcome(fmt.Sprintf("upstream error: %s %s", event.ErrorCode, event.ErrorMsg))
			}
			continue
		}

		switch event.EventType {
		case "assistantResponseEvent":
			text := ParseAssistantResponsePayload(event.Payload)
			if text != "" {
				contentBlocks = append(contentBlocks, map[string]string{
					"type": "text",
					"text": text,
				})
				outputTokens += estimateTokens(text)
			}
		case "toolUseEvent":
			tu, err := ParseToolUsePayload(event.Payload)
			if err != nil {
				continue
			}
			if tu.Stop {
				stopReason = "tool_use"
				name := tu.Name
				if original, ok := convertCtx.ToolNameMap[name]; ok {
					name = original
				}
				var input any
				if tu.Input != "" {
					json.Unmarshal([]byte(tu.Input), &input)
				}
				if input == nil {
					input = map[string]any{}
				}
				contentBlocks = append(contentBlocks, map[string]any{
					"type":  "tool_use",
					"id":    tu.ToolUseID,
					"name":  name,
					"input": input,
				})
			}
		case "contextUsageEvent":
			cu, _ := ParseContextUsagePayload(event.Payload)
			if cu != nil {
				inputTokens = int(cu.ContextUsagePercentage * float64(convertCtx.ContextWindow) / 100.0)
			}
		}
	}

	response := map[string]any{
		"id":          "msg_" + uuid.New().String()[:24],
		"type":        "message",
		"role":        "assistant",
		"model":       convertCtx.AnthropicModel,
		"content":     contentBlocks,
		"stop_reason": stopReason,
		"usage": map[string]int{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}

	respBody, _ := json.Marshal(response)

	if w != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBody)
	}

	usage := newTokenUsage(convertCtx.AnthropicModel, inputTokens, outputTokens, 0, time.Since(start).Milliseconds())

	applyCacheToUsage(usage, convertCtx)
	fillUsageCost(usage)

	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK, Body: respBody},
		Usage:    usage,
	}
}

func findThinkingStartTag(s string) int {
	idx := strings.Index(s, "<thinking>")
	if idx < 0 {
		return -1
	}
	// 检查前一个字符是否是引用字符
	if idx > 0 && isQuoteChar(s[idx-1]) {
		return -1
	}
	// 检查后一个字符
	afterIdx := idx + len("<thinking>")
	if afterIdx < len(s) && isQuoteChar(s[afterIdx]) {
		return -1
	}
	return idx
}

func findThinkingEndTag(s string) int {
	const tag = "</thinking>"
	search := 0
	for {
		idx := strings.Index(s[search:], tag)
		if idx < 0 {
			return -1
		}
		absIdx := search + idx

		if absIdx > 0 && isQuoteChar(s[absIdx-1]) {
			search = absIdx + 1
			continue
		}
		afterIdx := absIdx + len(tag)
		if afterIdx < len(s) && isQuoteChar(s[afterIdx]) {
			search = absIdx + 1
			continue
		}

		// 检查后面是否有 \n\n
		if afterIdx+2 <= len(s) && s[afterIdx:afterIdx+2] == "\n\n" {
			return absIdx
		}

		// 如果在缓冲区末尾且后面全是空白，也算
		if strings.TrimSpace(s[afterIdx:]) == "" {
			return absIdx
		}

		search = absIdx + 1
	}
}

func isQuoteChar(c byte) bool {
	switch c {
	case '`', '"', '\'', '\\', '#', '!', '@', '$', '%', '^', '&', '*',
		'(', ')', '-', '_', '=', '+', '[', ']', '{', '}', ';', ':',
		'<', '>', ',', '.', '?', '/':
		return true
	}
	return false
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// runeAlignBack returns the largest position <= pos that is a UTF-8 rune boundary.
func runeAlignBack(s string, pos int) int {
	for pos > 0 && !utf8.RuneStart(s[pos]) {
		pos--
	}
	return pos
}

func estimateTokens(text string) int {
	units := 0.0
	for _, r := range text {
		if r > 0x7F {
			units += 4.0
		} else {
			units += 1.0
		}
	}
	tokens := int(units / 4.0)
	if tokens < 1 && len(text) > 0 {
		tokens = 1
	}
	return tokens
}
