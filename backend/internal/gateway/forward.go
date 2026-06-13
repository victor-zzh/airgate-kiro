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
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func (g *KiroGateway) forwardHTTP(ctx context.Context, req *sdk.ForwardRequest, logger *slog.Logger) (sdk.ForwardOutcome, error) {
	path := resolveRequestPath(req)

	switch path {
	case "/v1/models":
		return g.handleModelsRequest(req), nil
	case "/v1/messages/count_tokens":
		return g.handleCountTokens(req), nil
	}

	// Web search: when web_search is the sole tool, call Kiro MCP instead of generateAssistantResponse
	if hasWebSearchTool(req.Body) {
		logger.Info("detected web_search tool, routing to MCP handler")
		return g.handleWebSearch(ctx, req, logger)
	}

	// /v1/messages
	switch req.Account.Type {
	case "oauth", "idc":
		return g.forwardOAuth(ctx, req, logger)
	case "api_key":
		return g.forwardAPIKey(ctx, req, logger)
	default:
		return accountDeadOutcome(fmt.Sprintf("unsupported account type: %s", req.Account.Type)), nil
	}
}

func (g *KiroGateway) forwardOAuth(ctx context.Context, req *sdk.ForwardRequest, logger *slog.Logger) (sdk.ForwardOutcome, error) {
	start := time.Now()

	updatedCreds, err := g.tokenMgr.ensureValidToken(ctx, req.Account)
	if err != nil {
		if errors.Is(err, ErrAccountDead) {
			logger.Error("account dead: refresh token invalid", "error", err)
			return accountDeadOutcome(err.Error()), nil
		}
		logger.Warn("token refresh failed, trying with current token", "error", err)
	}

	accessToken := req.Account.Credentials["access_token"]
	if accessToken == "" {
		return accountDeadOutcome("no access_token available"), nil
	}

	outcome := g.doForward(ctx, req, logger, start)

	// upstream 401/403 且提示 bearer token 无效 → force-refresh 后重试一次
	if outcome.Kind == sdk.OutcomeAccountDead && isBearerTokenInvalidResponse(outcome) {
		logger.Info("bearer token rejected by upstream, force-refreshing")
		retryCreds, refreshErr := g.tokenMgr.forceRefresh(ctx, req.Account)
		if refreshErr != nil {
			logger.Warn("force-refresh after 401/403 failed", "error", refreshErr)
		} else if req.Account.Credentials["access_token"] != "" {
			logger.Info("force-refresh succeeded, retrying request")
			if retryCreds != nil {
				updatedCreds = retryCreds
			}
			outcome = g.doForward(ctx, req, logger, start)
		}
	}

	if updatedCreds != nil {
		outcome.UpdatedCredentials = updatedCreds
	}
	outcome.Duration = time.Since(start)
	return outcome, nil
}

func (g *KiroGateway) forwardAPIKey(ctx context.Context, req *sdk.ForwardRequest, logger *slog.Logger) (sdk.ForwardOutcome, error) {
	start := time.Now()

	apiKey := req.Account.Credentials["kiro_api_key"]
	if apiKey == "" {
		return accountDeadOutcome("missing kiro_api_key"), nil
	}

	outcome := g.doForward(ctx, req, logger, start)
	outcome.Duration = time.Since(start)
	return outcome, nil
}

func (g *KiroGateway) doForward(ctx context.Context, req *sdk.ForwardRequest, logger *slog.Logger, start time.Time) sdk.ForwardOutcome {
	// 协议转换
	cfg := convertConfig{
		ProfileArn: req.Account.Credentials["profile_arn"],
	}
	kiroBody, convCtx, err := convertRequest(req.Body, req.Account, cfg, logger)
	if err != nil {
		logger.Warn("convert request failed", "error", err)
		errBody, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": err.Error(),
			},
		})
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       errBody,
			},
			Reason: err.Error(),
		}
	}

	region := resolveRegion(req.Account, g.ctx)
	machineID := resolveMachineID(req.Account)
	headers := buildKiroHeaders(req.Account, region, machineID, g.headerCfg)
	url := fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", region)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(kiroBody))
	if err != nil {
		return transientOutcome("build request: " + err.Error())
	}
	httpReq.Header = headers

	logger.Info("forwarding to kiro", "url", url, "model", convCtx.KiroModelID,
		"body_bytes", len(kiroBody),
		"body_mb", fmt.Sprintf("%.2f", float64(len(kiroBody))/1024/1024),
	)
	logger.Debug("kiro_request_headers",
		"content_type", httpReq.Header.Get("Content-Type"),
		"x_amzn_kiro_agent_mode", httpReq.Header.Get("x-amzn-kiro-agent-mode"),
		"x_amz_user_agent", httpReq.Header.Get("x-amz-user-agent"),
		"has_authorization", httpReq.Header.Get("Authorization") != "",
		"tokentype", httpReq.Header.Get("tokentype"),
		"host", httpReq.Header.Get("Host"),
	)

	streamable := req.Stream && req.Writer != nil
	resp, cancel, err := g.doStreamableUpstream(ctx, httpReq, streamable)
	if err != nil {
		return transientOutcome("upstream request failed: " + err.Error())
	}
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return g.handleErrorResponse(resp, req, logger)
	}

	// 成功响应 - 解码 Event Stream
	if req.Stream && req.Writer != nil {
		return streamKiroToSSE(ctx, resp.Body, req.Writer, convCtx, start)
	}
	return bufferKiroResponse(ctx, resp.Body, req.Writer, convCtx, start)
}

const (
	defaultFirstByteTimeout  = 60 * time.Second
	defaultStreamIdleTimeout = 60 * time.Second
)

// firstByteTimeout 流式等首响应头上限（可经 config first_byte_timeout 覆盖）。
func (g *KiroGateway) firstByteTimeout() time.Duration {
	if g == nil || g.ctx == nil || g.ctx.Config() == nil {
		return defaultFirstByteTimeout
	}
	if d := g.ctx.Config().GetDuration("first_byte_timeout"); d > 0 {
		return d
	}
	return defaultFirstByteTimeout
}

// streamIdleTimeout 流式读空闲上限（可经 config stream_idle_timeout 覆盖）。
func (g *KiroGateway) streamIdleTimeout() time.Duration {
	if g == nil || g.ctx == nil || g.ctx.Config() == nil {
		return defaultStreamIdleTimeout
	}
	if d := g.ctx.Config().GetDuration("stream_idle_timeout"); d > 0 {
		return d
	}
	return defaultStreamIdleTimeout
}

// doStreamableUpstream 执行上游请求并统一管理流式超时。
//
// stream=true 时：去掉总超时（避免仍在持续输出的长 Event Stream 被"总耗时"掐断），
// 首字节计时器仅约束"等响应头"阶段（头到即解除），返回的 resp.Body 被读空闲守卫包装——
// 流持续输出就不掐断，连续静默超过 stream_idle_timeout 才中止。
// stream=false 时：沿用共享 client 的总超时（720s），行为不变。
// 调用方必须 defer 返回的 cancel；err != nil 时 cancel 已被调用且返回 nil。
func (g *KiroGateway) doStreamableUpstream(ctx context.Context, httpReq *http.Request, stream bool) (*http.Response, context.CancelFunc, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	httpReq = httpReq.WithContext(reqCtx)

	var firstByteTimer *time.Timer
	if stream {
		firstByteTimer = time.AfterFunc(g.firstByteTimeout(), cancel)
	}

	client := g.client
	if stream {
		// 复制一份 client 去掉总超时，共用底层 Transport（连接池）。
		c := *g.client
		c.Timeout = 0
		client = &c
	}
	resp, err := client.Do(httpReq)
	if firstByteTimer != nil {
		firstByteTimer.Stop()
	}
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if stream {
		resp.Body = newStallGuardBody(resp.Body, g.streamIdleTimeout(), cancel)
	}
	return resp, cancel, nil
}

func (g *KiroGateway) handleErrorResponse(resp *http.Response, req *sdk.ForwardRequest, logger *slog.Logger) sdk.ForwardOutcome {
	body, _ := io.ReadAll(resp.Body)
	message := truncateString(string(body), 1000)
	retryAfter := extractRetryAfter(resp.Header)

	kind := classifyHTTPFailure(resp.StatusCode, message)
	logger.Warn("upstream error", "status", resp.StatusCode, "kind", kind.String(), "message", truncateString(message, 200))

	outcome := failureOutcome(resp.StatusCode, body, resp.Header.Clone(), message, retryAfter)

	return outcome
}

func (g *KiroGateway) handleModelsRequest(req *sdk.ForwardRequest) sdk.ForwardOutcome {
	body := buildModelsResponse()
	if req.Writer != nil {
		req.Writer.Header().Set("Content-Type", "application/json")
		req.Writer.WriteHeader(http.StatusOK)
		_, _ = req.Writer.Write(body)
	}
	return successOutcome(http.StatusOK, body, nil, nil)
}

func (g *KiroGateway) handleCountTokens(req *sdk.ForwardRequest) sdk.ForwardOutcome {
	// 本地估算 token 数
	tokens := estimateInputTokens(req.Body)
	respBody, _ := json.Marshal(map[string]int{"input_tokens": tokens})

	if req.Writer != nil {
		req.Writer.Header().Set("Content-Type", "application/json")
		req.Writer.WriteHeader(http.StatusOK)
		_, _ = req.Writer.Write(respBody)
	}
	return successOutcome(http.StatusOK, respBody, nil, nil)
}

func estimateInputTokens(body []byte) int {
	parsed := gjson.ParseBytes(body)
	total := 0.0

	// system prompt
	sys := parsed.Get("system").String()
	total += estimateCharUnits(sys)

	// messages
	for _, msg := range parsed.Get("messages").Array() {
		content := msg.Get("content")
		if content.Type == gjson.String {
			total += estimateCharUnits(content.String())
		} else if content.IsArray() {
			for _, block := range content.Array() {
				total += estimateCharUnits(block.Get("text").String())
			}
		}
	}

	// tools
	for _, tool := range parsed.Get("tools").Array() {
		total += estimateCharUnits(tool.Raw)
	}

	tokens := int(total / 4.0)

	// 小文本缩放
	switch {
	case tokens < 100:
		tokens = int(float64(tokens) * 1.5)
	case tokens < 200:
		tokens = int(float64(tokens) * 1.3)
	case tokens < 300:
		tokens = int(float64(tokens) * 1.25)
	case tokens < 800:
		tokens = int(float64(tokens) * 1.2)
	}

	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

func estimateCharUnits(s string) float64 {
	total := 0.0
	for _, r := range s {
		if r > 0x7F {
			total += 4.0
		} else {
			total += 1.0
		}
	}
	return total
}

func resolveRequestPath(req *sdk.ForwardRequest) string {
	if p := req.Headers.Get("X-Original-Path"); p != "" {
		return p
	}
	// 通过 body 内容推断
	if len(req.Body) > 0 {
		if gjson.GetBytes(req.Body, "messages").Exists() {
			return "/v1/messages"
		}
	}
	return "/v1/messages"
}
