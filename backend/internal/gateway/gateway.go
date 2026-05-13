package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const usageCacheTTL = 5 * time.Minute

type usageCacheEntry struct {
	quota      *quotaInfo
	capturedAt time.Time
}

// KiroGateway Kiro 反代网关插件。
type KiroGateway struct {
	logger        *slog.Logger
	ctx           sdk.PluginContext
	tokenMgr      *tokenManager
	client        *http.Client
	headerCfg     headerConfig
	oauthStore    *oauthSessionStore
	callbackLn    *callbackListener
	cancelCleanup context.CancelFunc
	usageCache    sync.Map // accountID (int64) -> *usageCacheEntry
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

	cleanupCtx, cancel := context.WithCancel(context.Background())
	g.cancelCleanup = cancel
	g.oauthStore.startCleanup(cleanupCtx)

	g.callbackLn = newCallbackListener(g.logger)
	g.callbackLn.start()

	g.logger.Info("kiro gateway initialized", "kiro_version", g.headerCfg.KiroVersion)
	return nil
}

func (g *KiroGateway) Start(ctx context.Context) error {
	g.logger.Info("kiro gateway started")
	return nil
}

func (g *KiroGateway) Stop(ctx context.Context) error {
	if g.cancelCleanup != nil {
		g.cancelCleanup()
	}
	if g.callbackLn != nil {
		g.callbackLn.stop()
	}
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
	accountType := inferAccountType(credentials)

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

func (g *KiroGateway) QueryQuota(ctx context.Context, credentials map[string]string) (*quotaInfo, error) {
	accountType := inferAccountType(credentials)

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

	quota, err := g.queryUsageLimits(ctx, account)
	if err != nil && accountType != "api_key" && isTokenInvalidError(err) {
		g.logger.Info("token rejected by upstream, forcing refresh", "error", err)
		if _, refreshErr := g.tokenMgr.forceRefresh(ctx, account); refreshErr != nil {
			return nil, refreshErr
		}
		quota, err = g.queryUsageLimits(ctx, account)
	}
	return quota, err
}

func (g *KiroGateway) queryUsageLimits(ctx context.Context, account *sdk.Account) (*quotaInfo, error) {
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
		NextDateReset    *float64 `json:"nextDateReset"`
		SubscriptionInfo *struct {
			SubscriptionTitle string `json:"subscriptionTitle"`
		} `json:"subscriptionInfo"`
		UsageBreakdownList []struct {
			CurrentUsage  float64  `json:"currentUsageWithPrecision"`
			UsageLimit    float64  `json:"usageLimitWithPrecision"`
			NextDateReset *float64 `json:"nextDateReset"`
			Bonuses       []struct {
				CurrentUsage float64 `json:"currentUsage"`
				UsageLimit   float64 `json:"usageLimit"`
				Status       string  `json:"status"`
			} `json:"bonuses"`
			FreeTrialInfo *struct {
				CurrentUsage    float64  `json:"currentUsageWithPrecision"`
				UsageLimit      float64  `json:"usageLimitWithPrecision"`
				FreeTrialExpiry *float64 `json:"freeTrialExpiry"`
				FreeTrialStatus string   `json:"freeTrialStatus"`
			} `json:"freeTrialInfo"`
		} `json:"usageBreakdownList"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("usage limits parse error: %w", err)
	}

	quota := &quotaInfo{
		Currency: "requests",
		Extra:    map[string]string{},
	}
	if raw.SubscriptionInfo != nil && raw.SubscriptionInfo.SubscriptionTitle != "" {
		quota.Extra["subscription"] = raw.SubscriptionInfo.SubscriptionTitle
	}

	// 用量重置时间（优先使用 breakdown 级别，fallback 到顶层）
	var resetTS *float64
	if raw.NextDateReset != nil {
		resetTS = raw.NextDateReset
	}

	if len(raw.UsageBreakdownList) > 0 {
		bd := raw.UsageBreakdownList[0]
		totalLimit := bd.UsageLimit
		totalUsed := bd.CurrentUsage

		// 累加活跃 bonus 额度
		for _, bonus := range bd.Bonuses {
			if strings.EqualFold(bonus.Status, "ACTIVE") {
				totalLimit += bonus.UsageLimit
				totalUsed += bonus.CurrentUsage
				quota.Extra["bonus_limit"] = fmt.Sprintf("%.0f", bonus.UsageLimit)
				quota.Extra["bonus_used"] = fmt.Sprintf("%.0f", bonus.CurrentUsage)
			}
		}

		// 累加活跃免费试用额度
		if ft := bd.FreeTrialInfo; ft != nil && strings.EqualFold(ft.FreeTrialStatus, "ACTIVE") {
			totalLimit += ft.UsageLimit
			totalUsed += ft.CurrentUsage
			quota.Extra["free_trial_limit"] = fmt.Sprintf("%.0f", ft.UsageLimit)
			quota.Extra["free_trial_used"] = fmt.Sprintf("%.0f", ft.CurrentUsage)
			if ft.FreeTrialExpiry != nil {
				quota.Extra["free_trial_expires_at"] = time.Unix(int64(*ft.FreeTrialExpiry), 0).UTC().Format(time.RFC3339)
			}
		}

		quota.Total = totalLimit
		quota.Used = totalUsed
		quota.Remaining = totalLimit - totalUsed

		if bd.NextDateReset != nil {
			resetTS = bd.NextDateReset
		}
	}

	if resetTS != nil {
		resetTime := time.Unix(int64(*resetTS), 0).UTC().Format(time.RFC3339)
		quota.ExpiresAt = resetTime
		quota.Extra["usage_reset_at"] = resetTime
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
	case "oauth/status":
		return g.handlePollAutoCallback(ctx, body)
	case "usage/accounts":
		return g.handleUsageAccounts(ctx, body)
	case "usage/probe":
		return g.handleUsageProbe(ctx, body)
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

	autoCallback := g.callbackLn != nil && g.callbackLn.isRunning()

	resp, _ := json.Marshal(map[string]any{
		"authorize_url": result.AuthURL,
		"state":         result.SessionID,
		"auto_callback": autoCallback,
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

	// 自动回调轮询：callback_url 以 "poll:" 开头
	if strings.HasPrefix(raw.CallbackURL, "poll:") {
		sessionID := strings.TrimPrefix(raw.CallbackURL, "poll:")
		return g.handlePollAutoCallback(ctx, []byte(`{"session_id":"`+sessionID+`"}`))
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
		"credentials":  result.Credentials,
	})
	return http.StatusOK, nil, resp, nil
}

func (g *KiroGateway) handlePollAutoCallback(ctx context.Context, body []byte) (int, http.Header, []byte, error) {
	var raw struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.SessionID == "" {
		resp, _ := json.Marshal(map[string]string{"error": "missing session_id"})
		return http.StatusBadRequest, nil, resp, nil
	}

	if g.callbackLn == nil || !g.callbackLn.isRunning() {
		resp, _ := json.Marshal(map[string]any{"status": "unavailable"})
		return http.StatusOK, nil, resp, nil
	}

	sess, ok := g.oauthStore.get(raw.SessionID)
	if !ok {
		resp, _ := json.Marshal(map[string]string{"error": "session expired or not found"})
		return http.StatusBadRequest, nil, resp, nil
	}

	callbackURL, ok := g.callbackLn.getResult(sess.State)
	if !ok {
		resp, _ := json.Marshal(map[string]any{"status": "pending"})
		return http.StatusOK, nil, resp, nil
	}

	result, err := exchangeCallbackByURL(ctx, g.oauthStore, callbackURL, g.client)
	if err != nil {
		g.logger.Warn("auto-callback exchange failed", "error", err)
		resp, _ := json.Marshal(map[string]string{"error": err.Error()})
		return http.StatusBadRequest, nil, resp, nil
	}

	if result.Continuation {
		g.logger.Info("auto-callback triggered BuilderID device auth", "user_code", result.UserCode)
		resp, _ := json.Marshal(map[string]any{
			"status":       "device_auth",
			"account_type": "__device_auth__",
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
		"status":       "complete",
		"account_type": authMethod,
		"account_name": result.Email,
		"credentials":  result.Credentials,
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
		if strings.Contains(err.Error(), "请先在浏览器中完成授权") {
			resp, _ := json.Marshal(map[string]any{"status": "pending"})
			return http.StatusOK, nil, resp, nil
		}
		g.logger.Warn("device token poll failed", "error", err)
		resp, _ := json.Marshal(map[string]string{"error": err.Error()})
		return http.StatusBadRequest, nil, resp, nil
	}

	authMethod := result.Credentials["auth_method"]
	if authMethod == "" {
		authMethod = "idc"
	}

	resp, _ := json.Marshal(map[string]any{
		"status":       "complete",
		"account_type": authMethod,
		"account_name": result.Email,
		"credentials":  result.Credentials,
	})
	return http.StatusOK, nil, resp, nil
}

// ── Usage 用量窗口 ──

func (g *KiroGateway) handleUsageAccounts(ctx context.Context, body []byte) (int, http.Header, []byte, error) {
	var accounts []struct {
		ID          int64             `json:"id"`
		Credentials map[string]string `json:"credentials"`
	}
	if err := json.Unmarshal(body, &accounts); err != nil {
		resp, _ := json.Marshal(map[string]string{"error": "invalid request body"})
		return http.StatusBadRequest, nil, resp, nil
	}

	now := time.Now()
	result := accountUsageAccountsResponse{
		Accounts: make(map[string]accountUsageInfo),
	}

	for _, a := range accounts {
		quota := g.getUsageCached(ctx, a.ID, a.Credentials)
		if quota == nil {
			continue
		}
		windows := g.buildUsageWindows(quota, now)
		if len(windows) == 0 {
			continue
		}
		result.Accounts[strconv.FormatInt(a.ID, 10)] = accountUsageInfo{
			UpdatedAt: now.UTC().Format(time.RFC3339),
			Windows:   windows,
		}
	}

	resp, _ := json.Marshal(result)
	return http.StatusOK, nil, resp, nil
}

func (g *KiroGateway) handleUsageProbe(ctx context.Context, body []byte) (int, http.Header, []byte, error) {
	var req struct {
		ID          int64             `json:"id"`
		Credentials map[string]string `json:"credentials"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.ID == 0 {
		resp, _ := json.Marshal(map[string]string{"error": "invalid request body"})
		return http.StatusBadRequest, nil, resp, nil
	}

	quota := g.probeUsage(ctx, req.ID, req.Credentials)
	if quota == nil {
		resp, _ := json.Marshal(map[string]string{"error": "usage probe failed"})
		return http.StatusInternalServerError, nil, resp, nil
	}

	now := time.Now()
	windows := g.buildUsageWindows(quota, now)
	info := accountUsageInfo{
		UpdatedAt: now.UTC().Format(time.RFC3339),
		Windows:   windows,
	}

	resp, _ := json.Marshal(accountUsageAccountsResponse{
		Accounts: map[string]accountUsageInfo{
			strconv.FormatInt(req.ID, 10): info,
		},
	})
	return http.StatusOK, nil, resp, nil
}

func (g *KiroGateway) getUsageCached(ctx context.Context, id int64, credentials map[string]string) *quotaInfo {
	if val, ok := g.usageCache.Load(id); ok {
		entry := val.(*usageCacheEntry)
		if time.Since(entry.capturedAt) < usageCacheTTL {
			return entry.quota
		}
	}
	return g.probeUsage(ctx, id, credentials)
}

func (g *KiroGateway) probeUsage(ctx context.Context, id int64, credentials map[string]string) *quotaInfo {
	accountType := inferAccountType(credentials)
	account := &sdk.Account{
		ID:          id,
		Type:        accountType,
		Credentials: credentials,
	}

	if accountType != "api_key" {
		if _, err := g.tokenMgr.ensureValidToken(ctx, account); err != nil {
			g.logger.Debug("usage probe token refresh failed", "account_id", id, "error", err)
			return nil
		}
	}

	quota, err := g.queryUsageLimits(ctx, account)
	if err != nil && accountType != "api_key" && isTokenInvalidError(err) {
		g.logger.Info("usage probe token rejected, forcing refresh", "account_id", id)
		if _, refreshErr := g.tokenMgr.forceRefresh(ctx, account); refreshErr != nil {
			g.logger.Debug("usage probe force refresh failed", "account_id", id, "error", refreshErr)
			return nil
		}
		quota, err = g.queryUsageLimits(ctx, account)
	}
	if err != nil {
		g.logger.Debug("usage probe query failed", "account_id", id, "error", err)
		return nil
	}

	g.usageCache.Store(id, &usageCacheEntry{
		quota:      quota,
		capturedAt: time.Now(),
	})
	return quota
}

func (g *KiroGateway) buildUsageWindows(quota *quotaInfo, now time.Time) []accountUsageWindow {
	if quota == nil || quota.Total <= 0 {
		return nil
	}

	usedPercent := (quota.Used / quota.Total) * 100

	label := fmt.Sprintf("Cr %d/%d", int64(math.Round(quota.Used)), int64(math.Round(quota.Total)))

	var resetAt *time.Time
	if quota.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, quota.ExpiresAt); err == nil {
			resetAt = &t
		}
	}

	return []accountUsageWindow{
		newAccountUsageWindow("monthly", label, usedPercent, resetAt, now),
	}
}

func normalizePlanName(raw string) string {
	words := strings.Fields(raw)
	for i, w := range words {
		if len(w) == 0 {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(words, " ")
}

func formatUsageNumber(n float64) string {
	if n == float64(int64(n)) {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'f', 2, 64)
}

func formatUsageCompact(n float64) string {
	i := int64(math.Round(n))
	if i >= 1000 {
		k := float64(i) / 1000
		if k == float64(int64(k)) {
			return fmt.Sprintf("%dK", int64(k))
		}
		return fmt.Sprintf("%.1fK", k)
	}
	return strconv.FormatInt(i, 10)
}
