package gateway

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

var ErrAccountDead = errors.New("account dead")

func classifyHTTPFailure(statusCode int, message string) sdk.OutcomeKind {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return sdk.OutcomeAccountRateLimited
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return sdk.OutcomeAccountDead
	case statusCode == http.StatusPaymentRequired && strings.Contains(message, "MONTHLY_REQUEST_COUNT"):
		return sdk.OutcomeAccountDead
	case statusCode == http.StatusBadRequest && containsAccountDisabledKeyword(message):
		return sdk.OutcomeAccountDead
	case statusCode >= 500:
		return sdk.OutcomeUpstreamTransient
	default:
		return sdk.OutcomeClientError
	}
}

func containsAccountDisabledKeyword(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "disabled") ||
		strings.Contains(lower, "deactivated") ||
		strings.Contains(lower, "suspended")
}

func isTokenInvalidError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "bearer token") ||
		strings.Contains(msg, "token is invalid") ||
		strings.Contains(msg, "invalid token") ||
		strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403")
}

func isNonRetryableRefreshError(err error) bool {
	msg := strings.ToLower(err.Error())
	// kiro.rs: 仅当 invalid_grant + "invalid refresh token" 同时出现才判死
	if strings.Contains(msg, "invalid_grant") {
		return strings.Contains(msg, "invalid refresh token")
	}
	for _, keyword := range []string{
		"expired_token",
		"unauthorized_client",
		"invalid_client",
		"access_denied",
	} {
		if strings.Contains(msg, keyword) {
			return true
		}
	}
	return false
}

func isBearerTokenInvalidResponse(outcome sdk.ForwardOutcome) bool {
	sc := outcome.Upstream.StatusCode
	if sc != http.StatusUnauthorized && sc != http.StatusForbidden {
		return false
	}
	msg := strings.ToLower(string(outcome.Upstream.Body))
	return strings.Contains(msg, "bearer token") ||
		strings.Contains(msg, "token is invalid") ||
		strings.Contains(msg, "invalid token") ||
		strings.Contains(msg, "unrecognized client") ||
		strings.Contains(msg, "the security token included in the request is invalid")
}

func inferAccountType(credentials map[string]string) string {
	if t := credentials["type"]; t != "" {
		if t == "idc" {
			return "oauth"
		}
		return t
	}
	if credentials["kiro_api_key"] != "" {
		return "api_key"
	}
	return "oauth"
}

func extractRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
