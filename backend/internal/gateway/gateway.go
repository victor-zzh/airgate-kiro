package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

// KiroGateway Kiro 反代网关插件。
type KiroGateway struct {
	logger     *slog.Logger
	ctx        sdk.PluginContext
	tokenMgr   *tokenManager
	client     *http.Client
	headerCfg  headerConfig
	oauthStore *oauthSessionStore
}

func (g *KiroGateway) Info() sdk.PluginInfo {
	return buildPluginInfo()
}

func (g *KiroGateway) Init(ctx sdk.PluginContext) error {
	g.logger = ctx.Logger()
	g.ctx = ctx
	g.headerCfg = defaultHeaderConfig(ctx)
	g.tokenMgr = newTokenManager(g.logger, g.headerCfg)
	g.oauthStore = newOAuthSessionStore()
	g.client = &http.Client{
		Timeout: 720 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	g.logger.Info("kiro gateway initialized", "kiro_version", g.headerCfg.KiroVersion)
	return nil
}

func (g *KiroGateway) Start(ctx context.Context) error {
	g.logger.Info("kiro gateway started")
	return nil
}

func (g *KiroGateway) Stop(ctx context.Context) error {
	g.client.CloseIdleConnections()
	g.logger.Info("kiro gateway stopped")
	return nil
}

func (g *KiroGateway) Platform() string {
	return PluginPlatform
}

func (g *KiroGateway) Models() []sdk.ModelInfo {
	return allModelInfos()
}

func (g *KiroGateway) Routes() []sdk.RouteDefinition {
	return pluginRoutes()
}

func (g *KiroGateway) Forward(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	requestID := sdk.ExtractOrGenerateRequestID(req.Headers)
	logger := g.logger.With("request_id", requestID, "model", req.Model, "account_id", req.Account.ID)
	return g.forwardHTTP(ctx, req, logger)
}

func (g *KiroGateway) ValidateAccount(ctx context.Context, credentials map[string]string) error {
	accountType := credentials["type"]
	if accountType == "" {
		// 推断类型
		if credentials["kiro_api_key"] != "" {
			accountType = "api_key"
		} else if credentials["client_id"] != "" {
			accountType = "idc"
		} else {
			accountType = "oauth"
		}
	}

	account := &sdk.Account{
		Type:        accountType,
		Credentials: credentials,
	}

	switch accountType {
	case "api_key":
		return g.validateAPIKey(ctx, account)
	case "oauth", "idc":
		return g.validateOAuth(ctx, account)
	default:
		return fmt.Errorf("unsupported account type: %s", accountType)
	}
}

func (g *KiroGateway) validateAPIKey(ctx context.Context, account *sdk.Account) error {
	_, err := g.queryUsageLimits(ctx, account)
	return err
}

func (g *KiroGateway) validateOAuth(ctx context.Context, account *sdk.Account) error {
	_, err := g.tokenMgr.ensureValidToken(ctx, account)
	if err != nil {
		return err
	}
	if account.Credentials["access_token"] == "" {
		return fmt.Errorf("failed to obtain access_token")
	}
	return nil
}

func (g *KiroGateway) QueryQuota(ctx context.Context, credentials map[string]string) (*sdk.QuotaInfo, error) {
	accountType := credentials["type"]
	if accountType == "" {
		if credentials["kiro_api_key"] != "" {
			accountType = "api_key"
		} else if credentials["client_id"] != "" {
			accountType = "idc"
		} else {
			accountType = "oauth"
		}
	}

	account := &sdk.Account{
		Type:        accountType,
		Credentials: credentials,
	}

	// 确保 token 有效
	if accountType != "api_key" {
		if _, err := g.tokenMgr.ensureValidToken(ctx, account); err != nil {
			return nil, err
		}
	}

	return g.queryUsageLimits(ctx, account)
}

func (g *KiroGateway) queryUsageLimits(ctx context.Context, account *sdk.Account) (*sdk.QuotaInfo, error) {
	region := resolveRegion(account, g.ctx)
	machineID := resolveMachineID(account)
	host := fmt.Sprintf("q.%s.amazonaws.com", region)

	url := fmt.Sprintf("https://%s/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST", host)
	if arn := account.Credentials["profile_arn"]; arn != "" {
		url += "&profileArn=" + arn
	}

	token := account.Credentials["access_token"]
	if account.Type == "api_key" {
		token = account.Credentials["kiro_api_key"]
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	cfg := g.headerCfg
	req.Header.Set("x-amz-user-agent", fmt.Sprintf("aws-sdk-js/1.0.0 KiroIDE-%s-%s", cfg.KiroVersion, machineID))
	req.Header.Set("User-Agent", fmt.Sprintf(
		"aws-sdk-js/1.0.0 ua/2.1 os/%s lang/js md/nodejs#%s api/codewhispererruntime#1.0.0 m/N,E KiroIDE-%s-%s",
		cfg.SystemVersion, cfg.NodeVersion, cfg.KiroVersion, machineID,
	))
	req.Header.Set("Host", host)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Connection", "close")
	if account.Type == "api_key" {
		req.Header.Set("tokentype", "API_KEY")
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage limits request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage limits HTTP %d: %s", resp.StatusCode, truncateString(string(body), 500))
	}

	// 解析响应
	var raw struct {
		SubscriptionInfo struct {
			SubscriptionTitle string `json:"subscriptionTitle"`
		} `json:"subscriptionInfo"`
		UsageBreakdownList []struct {
			CurrentUsage              float64 `json:"currentUsageWithPrecision"`
			UsageLimit                float64 `json:"usageLimitWithPrecision"`
		} `json:"usageBreakdownList"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("usage limits parse error: %w", err)
	}

	quota := &sdk.QuotaInfo{
		Currency: "requests",
		Extra:    map[string]string{},
	}
	if raw.SubscriptionInfo.SubscriptionTitle != "" {
		quota.Extra["subscription"] = raw.SubscriptionInfo.SubscriptionTitle
	}
	if len(raw.UsageBreakdownList) > 0 {
		bd := raw.UsageBreakdownList[0]
		quota.Total = bd.UsageLimit
		quota.Used = bd.CurrentUsage
		quota.Remaining = bd.UsageLimit - bd.CurrentUsage
	}

	return quota, nil
}

func (g *KiroGateway) HandleWebSocket(ctx context.Context, conn sdk.WebSocketConn) (sdk.ForwardOutcome, error) {
	return sdk.ForwardOutcome{}, sdk.ErrNotSupported
}

// HandleRequest 处理插件自定义 RPC 请求（OAuth 授权流程）。
// Core 将 /api/v1/admin/plugins/kiro/rpc/* 透传到这里。
func (g *KiroGateway) HandleRequest(ctx context.Context, method, path, query string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	switch path {
	case "oauth/start":
		return g.handleGenerateAuthURL(ctx)
	case "oauth/exchange":
		return g.handleExchangeCallback(ctx, body)
	case "oauth/device-complete":
		return g.handleDeviceComplete(ctx, body)
	default:
		resp, _ := json.Marshal(map[string]string{"error": "not found"})
		return http.StatusNotFound, nil, resp, nil
	}
}

func (g *KiroGateway) handleGenerateAuthURL(ctx context.Context) (int, http.Header, []byte, error) {
	result, err := generateAuthURL(g.oauthStore)
	if err != nil {
		resp, _ := json.Marshal(map[string]string{"error": err.Error()})
		return http.StatusInternalServerError, nil, resp, nil
	}

	resp, _ := json.Marshal(map[string]string{
		"authorize_url": result.AuthURL,
		"state":         result.SessionID,
	})
	return http.StatusOK, nil, resp, nil
}

func (g *KiroGateway) handleExchangeCallback(ctx context.Context, body []byte) (int, http.Header, []byte, error) {
	var raw struct {
		CallbackURL string `json:"callback_url"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.CallbackURL == "" {
		resp, _ := json.Marshal(map[string]string{"error": "missing callback_url"})
		return http.StatusBadRequest, nil, resp, nil
	}

	// 设备授权轮询：callback_url 以 "device-complete:" 开头
	if strings.HasPrefix(raw.CallbackURL, "device-complete:") {
		sessionID := strings.TrimPrefix(raw.CallbackURL, "device-complete:")
		return g.handleDeviceComplete(ctx, []byte(`{"session_id":"`+sessionID+`"}`))
	}

	result, err := exchangeCallbackByURL(ctx, g.oauthStore, raw.CallbackURL, g.client)
	if err != nil {
		g.logger.Warn("oauth exchange failed", "error", err)
		resp, _ := json.Marshal(map[string]string{"error": err.Error()})
		return http.StatusBadRequest, nil, resp, nil
	}

	// BuilderID 设备授权续接
	if result.Continuation {
		g.logger.Info("BuilderID device auth started", "user_code", result.UserCode)
		resp, _ := json.Marshal(map[string]any{
			"account_type": "__device_auth__",
			"account_name": "",
			"credentials": map[string]string{
				"verification_uri": result.VerificationURI,
				"user_code":        result.UserCode,
				"session_id":       result.DeviceSessionID,
			},
		})
		return http.StatusOK, nil, resp, nil
	}

	authMethod := result.Credentials["auth_method"]
	if authMethod == "" {
		authMethod = "oauth"
	}

	resp, _ := json.Marshal(map[string]any{
		"account_type": authMethod,
		"account_name": result.Email,
		"credentials": result.Credentials,
	})
	return http.StatusOK, nil, resp, nil
}

func (g *KiroGateway) handleDeviceComplete(ctx context.Context, body []byte) (int, http.Header, []byte, error) {
	var raw struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.SessionID == "" {
		resp, _ := json.Marshal(map[string]string{"error": "missing session_id"})
		return http.StatusBadRequest, nil, resp, nil
	}

	result, err := pollDeviceToken(ctx, g.oauthStore, raw.SessionID, g.client)
	if err != nil {
		g.logger.Warn("device token poll failed", "error", err)
		resp, _ := json.Marshal(map[string]string{"error": err.Error()})
		return http.StatusBadRequest, nil, resp, nil
	}

	authMethod := result.Credentials["auth_method"]
	if authMethod == "" {
		authMethod = "idc"
	}

	resp, _ := json.Marshal(map[string]any{
		"account_type": authMethod,
		"account_name": result.Email,
		"credentials":  result.Credentials,
	})
	return http.StatusOK, nil, resp, nil
}
