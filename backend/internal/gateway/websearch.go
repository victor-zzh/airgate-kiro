package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const webSearchQueryPrefix = "Perform a web search for the query: "

// ── MCP request/response types ──

type mcpRequest struct {
	ID      string    `json:"id"`
	JSONRPC string    `json:"jsonrpc"`
	Method  string    `json:"method"`
	Params  mcpParams `json:"params"`
}

type mcpParams struct {
	Name      string       `json:"name"`
	Arguments mcpArguments `json:"arguments"`
}

type mcpArguments struct {
	Query string `json:"query"`
}

type mcpResponse struct {
	Error   *mcpError  `json:"error,omitempty"`
	ID      string     `json:"id"`
	JSONRPC string     `json:"jsonrpc"`
	Result  *mcpResult `json:"result,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type webSearchResults struct {
	Results      []webSearchResult `json:"results"`
	TotalResults *int              `json:"totalResults,omitempty"`
	Query        string            `json:"query,omitempty"`
	Error        string            `json:"error,omitempty"`
}

type webSearchResult struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Snippet       string `json:"snippet,omitempty"`
	PublishedDate *int64 `json:"publishedDate,omitempty"`
	Domain        string `json:"domain,omitempty"`
}

// ── Detection ──

// hasWebSearchTool returns true when the only tool in the request is web_search.
func hasWebSearchTool(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return false
	}
	arr := tools.Array()
	if len(arr) != 1 {
		return false
	}
	return arr[0].Get("name").String() == "web_search"
}

// isServerTool returns true for Anthropic server-side tools that Kiro doesn't understand.
func isServerTool(tool gjson.Result) bool {
	t := tool.Get("type").String()
	return t == "server_tool" || strings.HasPrefix(t, "web_search")
}

// extractSearchQuery extracts the search query from the last user message.
func extractSearchQuery(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}
	arr := messages.Array()
	if len(arr) == 0 {
		return ""
	}

	lastMsg := arr[len(arr)-1]
	content := lastMsg.Get("content")

	var text string
	if content.Type == gjson.String {
		text = content.String()
	} else if content.IsArray() {
		for _, block := range content.Array() {
			if block.Get("type").String() == "text" {
				text = block.Get("text").String()
				break
			}
		}
	}

	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, webSearchQueryPrefix) {
		text = strings.TrimPrefix(text, webSearchQueryPrefix)
	}
	return text
}

// ── MCP headers ──

func buildMCPHeaders(account *sdk.Account, region, machineID string, cfg headerConfig) http.Header {
	host := fmt.Sprintf("q.%s.amazonaws.com", region)
	xAmzUA := fmt.Sprintf("aws-sdk-js/1.0.34 KiroIDE-%s-%s", cfg.KiroVersion, machineID)
	ua := fmt.Sprintf(
		"aws-sdk-js/1.0.34 ua/2.1 os/%s lang/js md/nodejs#%s api/codewhispererruntime#1.0.0 m/N,E KiroIDE-%s-%s",
		cfg.SystemVersion, cfg.NodeVersion, cfg.KiroVersion, machineID,
	)

	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("x-amz-user-agent", xAmzUA)
	h.Set("User-Agent", ua)
	h.Set("Host", host)
	h.Set("amz-sdk-invocation-id", uuid.New().String())
	h.Set("amz-sdk-request", "attempt=1; max=3")

	token := account.Credentials["access_token"]
	if account.Type == "api_key" {
		token = account.Credentials["kiro_api_key"]
		h.Set("tokentype", "API_KEY")
	}
	h.Set("Authorization", "Bearer "+token)

	if arn := account.Credentials["profile_arn"]; arn != "" {
		h.Set("x-amzn-kiro-profile-arn", arn)
	}

	return h
}

// ── MCP call ──

func (g *KiroGateway) callMCP(ctx context.Context, req *sdk.ForwardRequest, query string, logger *slog.Logger) (*webSearchResults, error) {
	region := resolveRegion(req.Account, g.ctx)
	machineID := resolveMachineID(req.Account)
	url := fmt.Sprintf("https://q.%s.amazonaws.com/mcp", region)

	toolUseID := fmt.Sprintf("web_search_tooluse_%s_%d_%s",
		uuid.New().String()[:22], time.Now().UnixMilli(), uuid.New().String()[:8])

	mcpReq := mcpRequest{
		ID:      toolUseID,
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: mcpParams{
			Name:      "web_search",
			Arguments: mcpArguments{Query: query},
		},
	}

	body, err := json.Marshal(mcpReq)
	if err != nil {
		return nil, fmt.Errorf("marshal MCP request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build MCP request: %w", err)
	}
	httpReq.Header = buildMCPHeaders(req.Account, region, machineID, g.headerCfg)

	logger.Info("calling kiro MCP for web search", "url", url, "query", query)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("MCP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read MCP response: %w", err)
	}

	if resp.StatusCode >= 400 {
		msg := truncateString(string(respBody), 500)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, msg)
	}

	var mcpResp mcpResponse
	if err := json.Unmarshal(respBody, &mcpResp); err != nil {
		return nil, fmt.Errorf("parse MCP response: %w", err)
	}

	if mcpResp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", mcpResp.Error.Code, mcpResp.Error.Message)
	}

	if mcpResp.Result == nil || len(mcpResp.Result.Content) == 0 {
		return &webSearchResults{}, nil
	}

	var results webSearchResults
	for _, c := range mcpResp.Result.Content {
		if c.Type == "text" && c.Text != "" {
			if err := json.Unmarshal([]byte(c.Text), &results); err != nil {
				logger.Warn("parse web search results failed", "error", err, "text", truncateString(c.Text, 200))
				continue
			}
			break
		}
	}

	logger.Info("web search completed", "result_count", len(results.Results))
	return &results, nil
}

// ── Handler ──

func (g *KiroGateway) handleWebSearch(ctx context.Context, req *sdk.ForwardRequest, logger *slog.Logger) (sdk.ForwardOutcome, error) {
	query := extractSearchQuery(req.Body)
	if query == "" {
		return transientOutcome("no search query found in request"), nil
	}

	start := time.Now()

	var updatedCreds map[string]string
	switch req.Account.Type {
	case "oauth", "idc":
		creds, err := g.tokenMgr.ensureValidToken(ctx, req.Account)
		if err != nil {
			if errors.Is(err, ErrAccountDead) {
				return accountDeadOutcome(err.Error()), nil
			}
			logger.Warn("token refresh failed, trying with current token", "error", err)
		}
		updatedCreds = creds
		if req.Account.Credentials["access_token"] == "" {
			return accountDeadOutcome("no access_token available"), nil
		}
	case "api_key":
		if req.Account.Credentials["kiro_api_key"] == "" {
			return accountDeadOutcome("missing kiro_api_key"), nil
		}
	}

	results, err := g.callMCP(ctx, req, query, logger)

	// Token rejected → force-refresh and retry once
	if err != nil && (req.Account.Type == "oauth" || req.Account.Type == "idc") && isTokenInvalidError(err) {
		logger.Info("MCP token rejected, force-refreshing")
		retryCreds, refreshErr := g.tokenMgr.forceRefresh(ctx, req.Account)
		if refreshErr != nil {
			logger.Warn("force-refresh for MCP failed", "error", refreshErr)
		} else if req.Account.Credentials["access_token"] != "" {
			if retryCreds != nil {
				updatedCreds = retryCreds
			}
			results, err = g.callMCP(ctx, req, query, logger)
		}
	}

	if err != nil {
		logger.Warn("web search MCP call failed", "error", err)
		return transientOutcome("web search failed: " + err.Error()), nil
	}

	model := gjson.GetBytes(req.Body, "model").String()
	inputTokens := estimateInputTokens(req.Body)

	var outcome sdk.ForwardOutcome
	if req.Stream && req.Writer != nil {
		outcome = streamWebSearchSSE(req.Writer, results, query, model, inputTokens, start)
	} else {
		outcome = bufferWebSearchResponse(req.Writer, results, query, model, inputTokens, start)
	}

	if updatedCreds != nil {
		outcome.UpdatedCredentials = updatedCreds
	}
	outcome.Duration = time.Since(start)
	return outcome, nil
}

// ── SSE synthesis ──

func streamWebSearchSSE(w http.ResponseWriter, results *webSearchResults, query, model string, inputTokens int, start time.Time) sdk.ForwardOutcome {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	emit := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	msgID := "msg_" + uuid.New().String()[:24]
	toolUseID := "srvtoolu_" + uuid.New().String()[:24]
	outputTokens := 0

	// Event 1: message_start
	emit("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","model":"%s","content":[],"stop_reason":null,"usage":{"input_tokens":%d,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		msgID, model, inputTokens,
	))

	// Event 2-4: text block (index 0) - search intent
	searchText := fmt.Sprintf("I'll search for %s.", jsonString(query))
	outputTokens += estimateTokens(searchText)

	emit("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	emit("content_block_delta", fmt.Sprintf(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`,
		jsonString(searchText),
	))
	emit("content_block_stop", `{"type":"content_block_stop","index":0}`)

	// Event 5-6: server_tool_use block (index 1)
	inputJSON, _ := json.Marshal(map[string]string{"query": query})
	emit("content_block_start", fmt.Sprintf(
		`{"type":"content_block_start","index":1,"content_block":{"id":"%s","type":"server_tool_use","name":"web_search","input":%s}}`,
		toolUseID, string(inputJSON),
	))
	emit("content_block_stop", `{"type":"content_block_stop","index":1}`)

	// Event 7-8: web_search_tool_result block (index 2)
	resultContent := buildSearchResultContent(results)
	resultJSON, _ := json.Marshal(resultContent)
	emit("content_block_start", fmt.Sprintf(
		`{"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","content":%s}}`,
		string(resultJSON),
	))
	emit("content_block_stop", `{"type":"content_block_stop","index":2}`)

	// Event 9-11: text summary block (index 3) + message_delta + message_stop
	summary := buildSearchSummary(results)
	outputTokens += estimateTokens(summary)

	emit("content_block_start",
		`{"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}`)
	emit("content_block_delta", fmt.Sprintf(
		`{"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":%s}}`,
		jsonString(summary),
	))
	emit("content_block_stop", `{"type":"content_block_stop","index":3}`)

	emit("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d,"server_tool_use":{"web_search_requests":1}}}`,
		outputTokens,
	))
	emit("message_stop", `{"type":"message_stop"}`)

	usage := newTokenUsage(model, inputTokens, outputTokens, 0, time.Since(start).Milliseconds())
	fillUsageCost(usage)

	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK},
		Usage:    usage,
	}
}

// ── Buffered (non-streaming) response ──

func bufferWebSearchResponse(w http.ResponseWriter, results *webSearchResults, query, model string, inputTokens int, start time.Time) sdk.ForwardOutcome {
	msgID := "msg_" + uuid.New().String()[:24]
	toolUseID := "srvtoolu_" + uuid.New().String()[:24]

	searchText := fmt.Sprintf("I'll search for %s.", jsonString(query))
	summary := buildSearchSummary(results)
	outputTokens := estimateTokens(searchText) + estimateTokens(summary)

	contentBlocks := []any{
		map[string]any{"type": "text", "text": searchText},
		map[string]any{
			"type":  "server_tool_use",
			"id":    toolUseID,
			"name":  "web_search",
			"input": map[string]string{"query": query},
		},
		map[string]any{
			"type":    "web_search_tool_result",
			"content": buildSearchResultContent(results),
		},
		map[string]any{"type": "text", "text": summary},
	}

	response := map[string]any{
		"id":          msgID,
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     contentBlocks,
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":    inputTokens,
			"output_tokens":   outputTokens,
			"server_tool_use": map[string]int{"web_search_requests": 1},
		},
	}

	respBody, _ := json.Marshal(response)

	if w != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBody)
	}

	usage := newTokenUsage(model, inputTokens, outputTokens, 0, time.Since(start).Milliseconds())
	fillUsageCost(usage)

	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK, Body: respBody},
		Usage:    usage,
	}
}

// ── Helpers ──

func buildSearchResultContent(results *webSearchResults) []map[string]any {
	if len(results.Results) == 0 {
		return []map[string]any{}
	}
	content := make([]map[string]any, 0, len(results.Results))
	for _, r := range results.Results {
		entry := map[string]any{
			"type":  "web_search_result",
			"title": r.Title,
			"url":   r.URL,
		}
		if r.Snippet != "" {
			entry["encrypted_content"] = r.Snippet
		}
		if r.PublishedDate != nil {
			t := time.UnixMilli(*r.PublishedDate)
			entry["page_age"] = t.Format("January 2, 2006")
		}
		content = append(content, entry)
	}
	return content
}

func buildSearchSummary(results *webSearchResults) string {
	if len(results.Results) == 0 {
		return "No search results found."
	}

	var sb strings.Builder
	sb.WriteString("Here are the search results:\n\n")
	for i, r := range results.Results {
		fmt.Fprintf(&sb, "%d. [%s](%s)", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			sb.WriteString("\n   ")
			sb.WriteString(r.Snippet)
		}
		sb.WriteString("\n\n")
	}
	return sb.String()
}
