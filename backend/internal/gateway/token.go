package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
	"github.com/google/uuid"
)

const (
	tokenExpireSkew      = 5 * time.Minute
	refreshCooldown      = 60 * time.Second
	refreshMaxRetries    = 2
	refreshRetryDelay    = 1 * time.Second
	defaultTokenLifetime = 60 * time.Minute
)

type tokenManager struct {
	logger *slog.Logger
	config headerConfig
	client *http.Client
	locks  sync.Map // accountID -> *accountRefreshState
}

type accountRefreshState struct {
	mu            sync.Mutex
	lastToken     string
	lastError     error
	lastErrorAt   time.Time
	latestCreds   map[string]string // 最新一次成功刷新的完整 credentials 快照
	latestCredsAt time.Time
}

type tokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ProfileArn   string `json:"profileArn,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
}

type idcTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
}

func newTokenManager(logger *slog.Logger, cfg headerConfig) *tokenManager {
	return &tokenManager{
		logger: logger,
		config: cfg,
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// ensureValidToken 检查 token 是否有效，过期则刷新。返回更新后的凭证 map（可为 nil）。
func (m *tokenManager) ensureValidToken(ctx context.Context, account *sdk.Account) (map[string]string, error) {
	if account.Type == "api_key" {
		return nil, nil
	}

	expiresAt := account.Credentials["expires_at"]
	if expiresAt != "" {
		t, err := time.Parse(time.RFC3339, expiresAt)
		if err == nil && time.Until(t) > tokenExpireSkew {
			return nil, nil
		}
	}

	return m.doRefresh(ctx, account)
}

// forceRefresh 跳过 expires_at 检查，强制刷新 token。
func (m *tokenManager) forceRefresh(ctx context.Context, account *sdk.Account) (map[string]string, error) {
	return m.doRefresh(ctx, account)
}

func (m *tokenManager) doRefresh(ctx context.Context, account *sdk.Account) (map[string]string, error) {
	if err := validateRefreshToken(account.Credentials["refresh_token"]); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAccountDead, err)
	}

	val, _ := m.locks.LoadOrStore(account.ID, &accountRefreshState{})
	state := val.(*accountRefreshState)

	state.mu.Lock()
	defer state.mu.Unlock()

	// 另一个 goroutine 已成功刷新且 token 比当前请求中的更新 → 直接复用
	if state.latestCreds != nil && state.lastError == nil &&
		state.lastToken != "" && state.lastToken != account.Credentials["access_token"] {
		m.logger.Info("reusing token refreshed by another goroutine", "account_id", account.ID)
		for k, v := range state.latestCreds {
			account.Credentials[k] = v
		}
		return state.latestCreds, nil
	}

	if state.lastToken != "" && state.lastToken == account.Credentials["access_token"] {
		if state.lastError != nil && time.Since(state.lastErrorAt) < refreshCooldown {
			m.logger.Warn("token refresh in cooldown", "account_id", account.ID)
			return nil, fmt.Errorf("%w: refresh in cooldown, last error: %v", ErrAccountDead, state.lastError)
		}
	}

	currentToken := account.Credentials["access_token"]
	oldRefreshToken := account.Credentials["refresh_token"]

	var lastErr error
	for attempt := range refreshMaxRetries + 1 {
		if attempt > 0 {
			time.Sleep(refreshRetryDelay)
		}

		var resp *tokenResponse
		var err error

		if account.Credentials["client_id"] != "" && account.Credentials["client_secret"] != "" {
			resp, err = m.refreshIdC(ctx, account)
		} else {
			resp, err = m.refreshSocial(ctx, account)
		}

		if err != nil {
			lastErr = err
			if isNonRetryableRefreshError(err) {
				m.logger.Error("non-retryable refresh error", "account_id", account.ID, "error", err)
				state.lastToken = currentToken
				state.lastError = err
				state.lastErrorAt = time.Now()
				state.latestCreds = nil
				return nil, fmt.Errorf("%w: %v", ErrAccountDead, err)
			}
			m.logger.Warn("refresh attempt failed", "account_id", account.ID, "attempt", attempt+1, "error", err)
			continue
		}

		if resp.AccessToken == "" {
			lastErr = fmt.Errorf("refresh returned empty access_token")
			m.logger.Warn("refresh returned empty access_token", "account_id", account.ID)
			continue
		}

		updated := map[string]string{
			"access_token": resp.AccessToken,
		}
		if resp.RefreshToken != "" {
			updated["refresh_token"] = resp.RefreshToken
			if resp.RefreshToken != oldRefreshToken {
				m.logger.Info("refresh_token rotated", "account_id", account.ID)
			}
		}
		if resp.ProfileArn != "" {
			updated["profile_arn"] = resp.ProfileArn
		}
		expiresAt := ""
		if resp.ExpiresIn > 0 {
			expiresAt = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).Format(time.RFC3339)
		} else {
			expiresAt = time.Now().Add(defaultTokenLifetime).Format(time.RFC3339)
		}
		updated["expires_at"] = expiresAt

		account.Credentials["access_token"] = resp.AccessToken
		if resp.RefreshToken != "" {
			account.Credentials["refresh_token"] = resp.RefreshToken
		}
		account.Credentials["expires_at"] = expiresAt
		if resp.ProfileArn != "" {
			account.Credentials["profile_arn"] = resp.ProfileArn
		}

		state.lastToken = resp.AccessToken
		state.lastError = nil
		state.latestCreds = updated
		state.latestCredsAt = time.Now()
		m.logger.Info("token refreshed", "account_id", account.ID)
		return updated, nil
	}

	state.lastToken = currentToken
	state.lastError = lastErr
	state.lastErrorAt = time.Now()
	state.latestCreds = nil
	return nil, fmt.Errorf("token refresh failed after %d retries: %v", refreshMaxRetries+1, lastErr)
}

func (m *tokenManager) refreshSocial(ctx context.Context, account *sdk.Account) (*tokenResponse, error) {
	region := resolveAuthRegion(account)
	machineID := resolveMachineID(account)
	url := fmt.Sprintf("https://prod.%s.auth.desktop.kiro.dev/refreshToken", region)

	body, _ := json.Marshal(map[string]string{
		"refreshToken": account.Credentials["refresh_token"],
	})

	host := fmt.Sprintf("prod.%s.auth.desktop.kiro.dev", region)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
	req.Header.Set("User-Agent", fmt.Sprintf("KiroIDE-%s-%s", m.config.KiroVersion, machineID))
	req.Header.Set("Host", host)
	req.Header.Set("Connection", "close")
	if m.config.KiroCommit != "" {
		req.Header.Set("x-amzn-kiro-commit", m.config.KiroCommit)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("social refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("social refresh HTTP %d: %s", resp.StatusCode, truncateString(string(respBody), 500))
	}

	result, err := parseSocialRefreshResponse(respBody)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func parseSocialRefreshResponse(respBody []byte) (*tokenResponse, error) {
	var result tokenResponse
	if err := json.Unmarshal(respBody, &result); err == nil && result.AccessToken != "" {
		return &result, nil
	}

	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &wrapper); err == nil && len(wrapper.Data) > 0 {
		if err := json.Unmarshal(wrapper.Data, &result); err == nil && result.AccessToken != "" {
			return &result, nil
		}
	}

	return nil, fmt.Errorf("social refresh: empty or unparseable accessToken in response: %s",
		truncateString(string(respBody), 300))
}

func (m *tokenManager) refreshIdC(ctx context.Context, account *sdk.Account) (*tokenResponse, error) {
	region := resolveAuthRegion(account)
	url := fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)

	body, _ := json.Marshal(map[string]string{
		"clientId":     account.Credentials["client_id"],
		"clientSecret": account.Credentials["client_secret"],
		"refreshToken": account.Credentials["refresh_token"],
		"grantType":    "refresh_token",
	})

	host := fmt.Sprintf("oidc.%s.amazonaws.com", region)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-amz-user-agent", "aws-sdk-js/3.980.0 KiroIDE")
	req.Header.Set("User-Agent", fmt.Sprintf(
		"aws-sdk-js/3.980.0 ua/2.1 os/%s lang/js md/nodejs#%s api/sso-oidc#3.980.0 m/E KiroIDE",
		m.config.SystemVersion, m.config.NodeVersion,
	))
	req.Header.Set("Host", host)
	req.Header.Set("amz-sdk-invocation-id", uuid.New().String())
	req.Header.Set("amz-sdk-request", "attempt=1; max=4")
	req.Header.Set("Connection", "close")

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("idc refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("idc refresh HTTP %d: %s", resp.StatusCode, truncateString(string(respBody), 500))
	}

	var idcResp idcTokenResponse
	if err := json.Unmarshal(respBody, &idcResp); err != nil {
		return nil, fmt.Errorf("idc refresh parse error: %w", err)
	}
	if idcResp.AccessToken == "" {
		return nil, fmt.Errorf("idc refresh returned empty access_token: %s",
			truncateString(string(respBody), 300))
	}

	return &tokenResponse{
		AccessToken:  idcResp.AccessToken,
		RefreshToken: idcResp.RefreshToken,
		ExpiresIn:    idcResp.ExpiresIn,
	}, nil
}

func validateRefreshToken(rt string) error {
	if rt == "" {
		return fmt.Errorf("refresh_token is empty")
	}
	if len(rt) < 100 {
		return fmt.Errorf("refresh_token too short (%d chars), likely truncated or invalid", len(rt))
	}
	if strings.Contains(rt, "...") {
		return fmt.Errorf("refresh_token contains truncation marker")
	}
	return nil
}

func resolveAuthRegion(account *sdk.Account) string {
	if r := account.Credentials["auth_region"]; r != "" {
		return r
	}
	if r := account.Credentials["region"]; r != "" {
		return r
	}
	return DefaultRegion
}
