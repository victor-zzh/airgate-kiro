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

	sdk "github.com/DouDOU-start/airgate-sdk"
	"github.com/tidwall/gjson"
)

func (g *KiroGateway) forwardHTTP(ctx context.Context, req *sdk.ForwardRequest, logger *slog.Logger) (sdk.ForwardOutcome, error) {
	path := resolveRequestPath(req)

	switch path {
	case "/v1/models":
		return g.handleModelsRequest(req), nil
	case "/v1/messages/count_tokens":
		return g.handleCountTokens(req), nil
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
	kiroBody, convCtx, err := convertRequest(req.Body, req.Account, cfg)
	if err != nil {
		logger.Warn("convert request failed", "error", err)
		// 模型不支持等客户端错误
		errBody, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": err.Error(),
			},
		})
		if req.Writer != nil {
			req.Writer.Header().Set("Content-Type", "application/json")
			req.Writer.WriteHeader(http.StatusBadRequest)
			req.Writer.Write(errBody)
		}
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadRequest,
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

	logger.Info("forwarding to kiro", "url", url, "model", convCtx.KiroModelID)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return transientOutcome("upstream request failed: " + err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return g.handleErrorResponse(resp, req, logger)
	}

	// 成功响应 - 解码 Event Stream
	if req.Stream && req.Writer != nil {
		return streamKiroToSSE(ctx, resp.Body, req.Writer, convCtx, start)
	}
	return bufferKiroResponse(ctx, resp.Body, req.Writer, convCtx, start)
}

func (g *KiroGateway) handleErrorResponse(resp *http.Response, req *sdk.ForwardRequest, logger *slog.Logger) sdk.ForwardOutcome {
	body, _ := io.ReadAll(resp.Body)
	message := truncateString(string(body), 1000)
	retryAfter := extractRetryAfter(resp.Header)

	kind := classifyHTTPFailure(resp.StatusCode, message)
	logger.Warn("upstream error", "status", resp.StatusCode, "kind", kind.String(), "message", truncateString(message, 200))

	outcome := failureOutcome(resp.StatusCode, body, resp.Header.Clone(), message, retryAfter)

	// ClientError: 透传给客户端
	if kind == sdk.OutcomeClientError && req.Writer != nil {
		for k, vs := range resp.Header {
			for _, v := range vs {
				req.Writer.Header().Add(k, v)
			}
		}
		req.Writer.WriteHeader(resp.StatusCode)
		req.Writer.Write(body)
	}

	return outcome
}

func (g *KiroGateway) handleModelsRequest(req *sdk.ForwardRequest) sdk.ForwardOutcome {
	body := buildModelsResponse()
	if req.Writer != nil {
		req.Writer.Header().Set("Content-Type", "application/json")
		req.Writer.WriteHeader(http.StatusOK)
		req.Writer.Write(body)
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
		req.Writer.Write(respBody)
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
