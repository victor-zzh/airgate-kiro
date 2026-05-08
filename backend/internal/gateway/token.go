package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

const (
	tokenExpireSkew    = 5 * time.Minute
	refreshCooldown    = 60 * time.Second
	refreshMaxRetries  = 2
	refreshRetryDelay  = 1 * time.Second
)

type tokenManager struct {
	logger *slog.Logger
	config headerConfig
	client *http.Client
	locks  sync.Map // accountID -> *accountRefreshState
}

type accountRefreshState struct {
	mu          sync.Mutex
	lastToken   string
	lastError   error
	lastErrorAt time.Time
}

type tokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ProfileArn   string `json:"profileArn,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
}

type idcTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
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
	val, _ := m.locks.LoadOrStore(account.ID, &accountRefreshState{})
	state := val.(*accountRefreshState)

	state.mu.Lock()
	defer state.mu.Unlock()

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
				return nil, fmt.Errorf("%w: %v", ErrAccountDead, err)
			}
			m.logger.Warn("refresh attempt failed", "account_id", account.ID, "attempt", attempt+1, "error", err)
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
		if resp.ExpiresIn > 0 {
			updated["expires_at"] = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).Format(time.RFC3339)
		}

		account.Credentials["access_token"] = resp.AccessToken
		if resp.RefreshToken != "" {
			account.Credentials["refresh_token"] = resp.RefreshToken
		}
		if resp.ExpiresIn > 0 {
			account.Credentials["expires_at"] = updated["expires_at"]
		}
		if resp.ProfileArn != "" {
			account.Credentials["profile_arn"] = resp.ProfileArn
		}

		state.lastToken = resp.AccessToken
		state.lastError = nil
		m.logger.Info("token refreshed", "account_id", account.ID)
		return updated, nil
	}

	state.lastToken = currentToken
	state.lastError = lastErr
	state.lastErrorAt = time.Now()
	return nil, fmt.Errorf("token refresh failed after %d retries: %v", refreshMaxRetries+1, lastErr)
}

func (m *tokenManager) refreshSocial(ctx context.Context, account *sdk.Account) (*tokenResponse, error) {
	region := resolveAuthRegion(account)
	machineID := resolveMachineID(account)
	url := fmt.Sprintf("https://prod.%s.auth.desktop.kiro.dev/refreshToken", region)

	body, _ := json.Marshal(map[string]string{
		"refreshToken": account.Credentials["refresh_token"],
	})

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", fmt.Sprintf("KiroIDE-%s-%s", m.config.KiroVersion, machineID))
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

	var result tokenResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("social refresh parse error: %w", err)
	}
	return &result, nil
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

	return &tokenResponse{
		AccessToken:  idcResp.AccessToken,
		RefreshToken: idcResp.RefreshToken,
		ExpiresIn:    idcResp.ExpiresIn,
	}, nil
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

