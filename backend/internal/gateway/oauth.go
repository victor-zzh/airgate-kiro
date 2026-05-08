package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	kiroSignInURL        = "https://app.kiro.dev/signin"
	kiroTokenExchangeURL = "https://prod.us-east-1.auth.desktop.kiro.dev/oauth/token"
	kiroCallbackBaseURL  = "http://localhost:3128"
	idcCallbackBaseURL = "http://127.0.0.1:3128"
	idcClientName   = "airgate-kiro"

	oauthSessionTTL = 30 * time.Minute
)

var idcScopes = []string{
	"sso:account:access",
	"codewhisperer:conversations",
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:taskassist",
}

// OAuthSession 浏览器 OAuth 授权会话。
type OAuthSession struct {
	State        string
	CodeVerifier string
	CreatedAt    time.Time
	// IdC 续接字段（BuilderID 设备授权时填充）
	ClientID     string
	ClientSecret string
	IDCRegion    string
	IssuerURL    string
	DeviceCode   string
}

// oauthSessionStore 内存中的 OAuth 会话存储。
type oauthSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*OAuthSession // sessionID -> session
}

func newOAuthSessionStore() *oauthSessionStore {
	return &oauthSessionStore{
		sessions: make(map[string]*OAuthSession),
	}
}

func (s *oauthSessionStore) put(sessionID string, sess *OAuthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = sess
	// 清理过期会话
	now := time.Now()
	for k, v := range s.sessions {
		if now.Sub(v.CreatedAt) > oauthSessionTTL {
			delete(s.sessions, k)
		}
	}
}

func (s *oauthSessionStore) get(sessionID string) (*OAuthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok || time.Since(sess.CreatedAt) > oauthSessionTTL {
		delete(s.sessions, sessionID)
		return nil, false
	}
	return sess, true
}

func (s *oauthSessionStore) remove(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

func (s *oauthSessionStore) startCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.mu.Lock()
				now := time.Now()
				for k, v := range s.sessions {
					if now.Sub(v.CreatedAt) > oauthSessionTTL {
						delete(s.sessions, k)
					}
				}
				s.mu.Unlock()
			}
		}
	}()
}

func (s *oauthSessionStore) findByState(state string) (string, *OAuthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, sess := range s.sessions {
		if now.Sub(sess.CreatedAt) > oauthSessionTTL {
			delete(s.sessions, id)
			continue
		}
		if sess.State == state {
			return id, sess, true
		}
	}
	return "", nil, false
}

// GenerateAuthURLResponse 生成授权链接的响应。
type GenerateAuthURLResponse struct {
	AuthURL     string `json:"auth_url"`
	SessionID   string `json:"session_id"`
	CallbackURL string `json:"callback_url"`
}

// ExchangeCallbackResponse 交换结果，包含完整凭证或 BuilderID 设备授权续接。
type ExchangeCallbackResponse struct {
	Credentials map[string]string `json:"credentials,omitempty"`
	Email       string            `json:"email,omitempty"`
	// BuilderID 设备授权续接
	Continuation        bool   `json:"-"`
	VerificationURI     string `json:"-"`
	UserCode            string `json:"-"`
	DeviceSessionID     string `json:"-"`
}

// generateAuthURL 生成 Kiro OAuth 授权链接。
func generateAuthURL(store *oauthSessionStore) (*GenerateAuthURLResponse, error) {
	state, err := randomBase64URL(32)
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	codeVerifier, err := randomBase64URL(48)
	if err != nil {
		return nil, fmt.Errorf("generate code_verifier: %w", err)
	}
	sessionID, err := randomBase64URL(16)
	if err != nil {
		return nil, fmt.Errorf("generate session_id: %w", err)
	}

	codeChallenge := computeS256Challenge(codeVerifier)

	store.put(sessionID, &OAuthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		CreatedAt:    time.Now(),
	})

	params := url.Values{
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"redirect_uri":          {kiroCallbackBaseURL},
		"redirect_from":         {"KiroIDE"},
	}

	authURL := kiroSignInURL + "?" + params.Encode()

	return &GenerateAuthURLResponse{
		AuthURL:     authURL,
		SessionID:   sessionID,
		CallbackURL: kiroCallbackBaseURL,
	}, nil
}

// exchangeCallbackByURL 通过 callback URL 中的 state 参数反查 session 并换 token。
func exchangeCallbackByURL(ctx context.Context, store *oauthSessionStore, rawCallbackURL string, httpClient *http.Client) (*ExchangeCallbackResponse, error) {
	callbackURL := normalizeCallbackURL(rawCallbackURL)
	parsed, err := url.Parse(callbackURL)
	if err != nil {
		return nil, fmt.Errorf("invalid callback URL: %w", err)
	}

	query := parsed.Query()

	if errParam := query.Get("error"); errParam != "" {
		errDesc := query.Get("error_description")
		return nil, fmt.Errorf("oauth error: %s - %s", errParam, errDesc)
	}

	state := query.Get("state")
	if state == "" {
		return nil, fmt.Errorf("missing state parameter in callback URL")
	}

	sessionID, sess, ok := store.findByState(state)
	if !ok {
		return nil, fmt.Errorf("session expired or not found for state")
	}
	defer store.remove(sessionID)

	loginOption := query.Get("login_option")
	if loginOption == "" {
		loginOption = "social"
	}

	code := query.Get("code")

	// ── 无 code：检查是否可启动 BuilderID 续接 ──
	if code == "" {
		switch strings.ToLower(loginOption) {
		case "builderid", "awsidc", "internal":
			return startBuilderIDContinuation(ctx, store, query, httpClient)
		case "external_idp":
			return nil, fmt.Errorf("外部 IdP 回调未提供授权码，无法自动导入")
		default:
			return nil, fmt.Errorf("回调 URL 中缺少授权码(code)，请确认已在浏览器中完成登录并复制了完整的地址栏 URL")
		}
	}

	// ── 有 code：判断是 IdC 续接还是 Kiro 社交登录 ──
	if sess.ClientID != "" {
		return exchangeIDCCode(ctx, code, sess, httpClient)
	}

	// 社交登录：走 Kiro token 端点
	callbackPath := parsed.Path
	if callbackPath == "" {
		callbackPath = "/oauth/callback"
	}
	redirectURI := kiroCallbackBaseURL + callbackPath + "?login_option=" + url.QueryEscape(loginOption)

	tokenResp, err := exchangeCodeForToken(ctx, code, sess.CodeVerifier, redirectURI, httpClient)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}

	creds := map[string]string{
		"access_token":  tokenResp.AccessToken,
		"refresh_token": tokenResp.RefreshToken,
		"auth_method":   resolveAuthMethod(loginOption, tokenResp),
	}
	if tokenResp.ProfileArn != "" {
		creds["profile_arn"] = tokenResp.ProfileArn
	}
	if tokenResp.ExpiresIn > 0 {
		creds["expires_at"] = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	if tokenResp.ClientID != "" {
		creds["client_id"] = tokenResp.ClientID
	}
	if tokenResp.ClientSecret != "" {
		creds["client_secret"] = tokenResp.ClientSecret
	}

	email := tokenResp.Email
	if email == "" {
		email = extractEmailFromJWT(tokenResp.AccessToken)
	}

	return &ExchangeCallbackResponse{
		Credentials: creds,
		Email:       email,
	}, nil
}

// ── BuilderID / IdC 直连授权 ──

// registerIDCClient 向 AWS SSO 注册 OIDC 公共客户端。
func registerIDCClient(ctx context.Context, region, issuerURL string, httpClient *http.Client) (clientID, clientSecret string, err error) {
	endpoint := fmt.Sprintf("https://oidc.%s.amazonaws.com/client/register", region)

	payload, _ := json.Marshal(map[string]any{
		"clientName":   idcClientName,
		"clientType":   "public",
		"grantTypes":   []string{"authorization_code", "refresh_token"},
		"issuerUrl":    issuerURL,
		"redirectUris": []string{idcCallbackBaseURL},
		"scopes":       idcScopes,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("IDC client registration request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("IDC client registration HTTP %d: %s", resp.StatusCode, truncateString(string(body), 500))
	}

	var result struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", fmt.Errorf("IDC registration response parse error: %w", err)
	}
	if result.ClientID == "" {
		return "", "", fmt.Errorf("IDC registration returned empty clientId")
	}

	return result.ClientID, result.ClientSecret, nil
}

// startBuilderIDContinuation 检测到 BuilderID 回调后，注册 IdC 客户端并发起设备授权。
func startBuilderIDContinuation(ctx context.Context, store *oauthSessionStore, query url.Values, httpClient *http.Client) (*ExchangeCallbackResponse, error) {
	issuerURL := query.Get("issuer_url")
	if issuerURL == "" {
		return nil, fmt.Errorf("BuilderID 回调缺少 issuer_url，无法自动注册 IdC 客户端")
	}
	idcRegion := query.Get("idc_region")
	if idcRegion == "" {
		idcRegion = DefaultRegion
	}

	clientID, clientSecret, err := registerIDCClient(ctx, idcRegion, issuerURL, httpClient)
	if err != nil {
		return nil, fmt.Errorf("BuilderID IdC 客户端注册失败: %w", err)
	}

	deviceResp, err := startDeviceAuthorization(ctx, idcRegion, clientID, clientSecret, issuerURL, httpClient)
	if err != nil {
		return nil, fmt.Errorf("BuilderID 设备授权启动失败: %w", err)
	}

	sessionID := "idc-device-" + deviceResp.UserCode
	store.put(sessionID, &OAuthSession{
		CreatedAt:    time.Now(),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		IDCRegion:    idcRegion,
		IssuerURL:    issuerURL,
		DeviceCode:   deviceResp.DeviceCode,
	})

	verificationURI := deviceResp.VerificationURIComplete
	if verificationURI == "" {
		verificationURI = deviceResp.VerificationURI
	}

	return &ExchangeCallbackResponse{
		Continuation:    true,
		VerificationURI: verificationURI,
		UserCode:        deviceResp.UserCode,
		DeviceSessionID: sessionID,
	}, nil
}

type deviceAuthResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

func startDeviceAuthorization(ctx context.Context, region, clientID, clientSecret, startURL string, httpClient *http.Client) (*deviceAuthResponse, error) {
	endpoint := fmt.Sprintf("https://oidc.%s.amazonaws.com/device_authorization", region)

	payload, _ := json.Marshal(map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"startUrl":     startURL,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device authorization request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("device authorization HTTP %d: %s", resp.StatusCode, truncateString(string(body), 500))
	}

	var result deviceAuthResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("device authorization response parse error: %w", err)
	}
	return &result, nil
}

// pollDeviceToken 轮询设备授权 token，用户完成浏览器授权后调用。
func pollDeviceToken(ctx context.Context, store *oauthSessionStore, sessionID string, httpClient *http.Client) (*ExchangeCallbackResponse, error) {
	sess, ok := store.get(sessionID)
	if !ok {
		return nil, fmt.Errorf("设备授权会话不存在或已过期，请重新开始")
	}
	if sess.DeviceCode == "" {
		return nil, fmt.Errorf("非设备授权会话")
	}

	endpoint := fmt.Sprintf("https://oidc.%s.amazonaws.com/token", sess.IDCRegion)

	payload, _ := json.Marshal(map[string]string{
		"clientId":     sess.ClientID,
		"clientSecret": sess.ClientSecret,
		"deviceCode":   sess.DeviceCode,
		"grantType":    "urn:ietf:params:oauth:grant-type:device_code",
	})

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device token poll failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// 检查是否还在等待用户授权
	if resp.StatusCode == http.StatusBadRequest {
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &errResp) == nil && errResp.Error == "authorization_pending" {
			return nil, fmt.Errorf("请先在浏览器中完成授权，然后再点击此按钮")
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("device token HTTP %d: %s", resp.StatusCode, truncateString(string(body), 500))
	}

	// 设备授权响应用 camelCase，刷新响应用 snake_case，兼容两种
	var tokenResp struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("device token response parse error: %w", err)
	}
	if tokenResp.AccessToken == "" {
		var snake idcTokenResponse
		if json.Unmarshal(body, &snake) == nil && snake.AccessToken != "" {
			tokenResp.AccessToken = snake.AccessToken
			tokenResp.RefreshToken = snake.RefreshToken
			tokenResp.ExpiresIn = snake.ExpiresIn
		}
	}
	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("device token response missing access_token")
	}

	store.remove(sessionID)

	creds := map[string]string{
		"access_token":  tokenResp.AccessToken,
		"refresh_token": tokenResp.RefreshToken,
		"client_id":     sess.ClientID,
		"client_secret": sess.ClientSecret,
		"auth_method":   "oauth",
		"region":        sess.IDCRegion,
	}
	if tokenResp.ExpiresIn > 0 {
		creds["expires_at"] = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	}

	return &ExchangeCallbackResponse{
		Credentials: creds,
		Email:       extractEmailFromJWT(tokenResp.AccessToken),
	}, nil
}

// exchangeIDCCode 通过 IdC OIDC token 端点交换授权码（保留用于可能的 Authorization Code 流程）。
func exchangeIDCCode(ctx context.Context, code string, sess *OAuthSession, httpClient *http.Client) (*ExchangeCallbackResponse, error) {
	endpoint := fmt.Sprintf("https://oidc.%s.amazonaws.com/token", sess.IDCRegion)
	redirectURI := idcCallbackBaseURL

	payload, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     sess.ClientID,
		"client_secret": sess.ClientSecret,
		"code":          code,
		"code_verifier": sess.CodeVerifier,
		"redirect_uri":  redirectURI,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("IDC token exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("IDC token exchange HTTP %d: %s", resp.StatusCode, truncateString(string(body), 500))
	}

	var tokenResp idcTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("IDC token response parse error: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("IDC token response missing access_token")
	}

	creds := map[string]string{
		"access_token":  tokenResp.AccessToken,
		"refresh_token": tokenResp.RefreshToken,
		"client_id":     sess.ClientID,
		"client_secret": sess.ClientSecret,
		"auth_method":   "oauth",
		"region":        sess.IDCRegion,
	}
	if tokenResp.ExpiresIn > 0 {
		creds["expires_at"] = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	}

	return &ExchangeCallbackResponse{
		Credentials: creds,
		Email:       extractEmailFromJWT(tokenResp.AccessToken),
	}, nil
}

type kiroTokenExchangeResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ProfileArn   string `json:"profileArn"`
	ExpiresIn    int64  `json:"expiresIn"`
	Email        string `json:"email"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func exchangeCodeForToken(ctx context.Context, code, codeVerifier, redirectURI string, httpClient *http.Client) (*kiroTokenExchangeResponse, error) {
	body, _ := json.Marshal(map[string]string{
		"code":          code,
		"code_verifier": codeVerifier,
		"redirect_uri":  redirectURI,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", kiroTokenExchangeURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "airgate-kiro OAuth")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange HTTP %d: %s", resp.StatusCode, truncateString(string(respBody), 500))
	}

	// 尝试解析，兼容 data 包装和直接返回两种格式
	var result kiroTokenExchangeResponse

	// 先尝试直接解析
	if err := json.Unmarshal(respBody, &result); err == nil && result.AccessToken != "" {
		return &result, nil
	}

	// 尝试从 data 字段提取
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &wrapper); err == nil && len(wrapper.Data) > 0 {
		if err := json.Unmarshal(wrapper.Data, &result); err == nil && result.AccessToken != "" {
			return &result, nil
		}
	}

	// 尝试 snake_case 字段名
	var snakeResult struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ProfileArn   string `json:"profile_arn"`
		ExpiresIn    int64  `json:"expires_in"`
		Email        string `json:"email"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(respBody, &snakeResult); err == nil && snakeResult.AccessToken != "" {
		return &kiroTokenExchangeResponse{
			AccessToken:  snakeResult.AccessToken,
			RefreshToken: snakeResult.RefreshToken,
			ProfileArn:   snakeResult.ProfileArn,
			ExpiresIn:    snakeResult.ExpiresIn,
			Email:        snakeResult.Email,
			ClientID:     snakeResult.ClientID,
			ClientSecret: snakeResult.ClientSecret,
		}, nil
	}

	return nil, fmt.Errorf("unable to parse token response: %s", truncateString(string(respBody), 300))
}

func resolveAuthMethod(loginOption string, token *kiroTokenExchangeResponse) string {
	return "oauth"
}

func normalizeCallbackURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	if strings.HasPrefix(raw, "/") {
		return kiroCallbackBaseURL + raw
	}
	if strings.HasPrefix(raw, "?") {
		return kiroCallbackBaseURL + "/oauth/callback" + raw
	}
	return raw
}

// extractEmailFromJWT 从 JWT token 的 claims 中提取邮箱。
func extractEmailFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 尝试标准 base64
		payload, err = base64.RawStdEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	for _, key := range []string{"email", "upn", "preferred_username", "login_hint"} {
		if v, ok := claims[key].(string); ok && v != "" && strings.Contains(v, "@") {
			return v
		}
	}
	return ""
}

func randomBase64URL(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func computeS256Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// ── Callback Listener ──

// callbackListener 在 localhost:3128 监听 OAuth 回调，自动捕获授权码。
type callbackListener struct {
	logger   *slog.Logger
	server   *http.Server
	running  bool
	mu       sync.Mutex
	captured sync.Map // state -> full callback URL
}

func newCallbackListener(logger *slog.Logger) *callbackListener {
	return &callbackListener{logger: logger}
}

func (cl *callbackListener) start() bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.running {
		return true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", cl.handleCallback)

	cl.server = &http.Server{
		Addr:    "127.0.0.1:3128",
		Handler: mux,
	}

	ln, err := net.Listen("tcp", cl.server.Addr)
	if err != nil {
		cl.logger.Warn("callback listener unavailable, manual paste required", "addr", cl.server.Addr, "error", err)
		return false
	}

	cl.running = true
	go func() {
		if err := cl.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			cl.logger.Warn("callback listener error", "error", err)
		}
		cl.mu.Lock()
		cl.running = false
		cl.mu.Unlock()
	}()

	cl.logger.Info("callback listener started", "addr", cl.server.Addr)
	return true
}

func (cl *callbackListener) stop() {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if !cl.running || cl.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cl.server.Shutdown(ctx)
	cl.running = false
}

func (cl *callbackListener) handleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}

	fullURL := kiroCallbackBaseURL + r.URL.RequestURI()
	cl.captured.Store(state, fullURL)

	statePrefix := state
	if len(statePrefix) > 8 {
		statePrefix = statePrefix[:8]
	}
	cl.logger.Info("oauth callback captured", "state_prefix", statePrefix)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(callbackSuccessHTML))
}

func (cl *callbackListener) getResult(state string) (string, bool) {
	val, ok := cl.captured.LoadAndDelete(state)
	if !ok {
		return "", false
	}
	return val.(string), true
}

func (cl *callbackListener) isRunning() bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.running
}

const callbackSuccessHTML = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>授权完成</title><style>
body{font-family:system-ui,-apple-system,sans-serif;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0;background:#f8f9fa}
.card{background:#fff;border-radius:12px;padding:2.5rem 3rem;box-shadow:0 2px 12px rgba(0,0,0,.08);text-align:center;max-width:400px}
.icon{font-size:3rem;margin-bottom:1rem}
h1{color:#16a34a;font-size:1.25rem;margin:0 0 .5rem;font-weight:600}
p{color:#6b7280;margin:0;line-height:1.5}
.hint{margin-top:1.25rem;font-size:.8rem;color:#9ca3af}
</style></head><body>
<div class="card">
<div class="icon">&#10003;</div>
<h1>Authorization Complete</h1>
<p>Credentials will be auto-filled in the Airgate panel.</p>
<p class="hint">You can close this window.</p>
</div>
<script>window.close()</script>
</body></html>`
