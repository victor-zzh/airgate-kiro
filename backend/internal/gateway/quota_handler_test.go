package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestHandleAccountQuota(t *testing.T) {
	g := &KiroGateway{
		headerCfg: defaultHeaderConfig(nil),
		tokenMgr:  newTokenManager(nil, defaultHeaderConfig(nil)),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "q.us-east-1.amazonaws.com" {
				t.Fatalf("host = %q, want q.us-east-1.amazonaws.com", req.URL.Host)
			}
			if req.Header.Get("Authorization") != "Bearer access-token" {
				t.Fatalf("Authorization = %q", req.Header.Get("Authorization"))
			}
			body := `{
				"nextDateReset": 1893456000,
				"subscriptionInfo": {"subscriptionTitle": "builder id pro"},
				"usageBreakdownList": [{
					"currentUsageWithPrecision": 25,
					"usageLimitWithPrecision": 100,
					"nextDateReset": 1893456000,
					"bonuses": [{"currentUsage": 5, "usageLimit": 10, "status": "ACTIVE"}],
					"freeTrialInfo": {
						"currentUsageWithPrecision": 2,
						"usageLimitWithPrecision": 8,
						"freeTrialStatus": "ACTIVE",
						"freeTrialExpiry": 1896048000
					}
				}]
			}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}

	reqBody, _ := json.Marshal(map[string]any{
		"id": int64(42),
		"credentials": map[string]string{
			"access_token":  "access-token",
			"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"email":         "dev@example.com",
			"refresh_token": "refresh-token",
		},
	})

	status, _, respBody, err := g.HandleRequest(context.Background(), http.MethodPost, "accounts/quota", "", nil, reqBody)
	if err != nil {
		t.Fatalf("HandleRequest returned error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, respBody)
	}

	var resp struct {
		ExpiresAt string            `json:"expires_at"`
		Extra     map[string]string `json:"extra"`
	}
	if err := json.NewDecoder(bytes.NewReader(respBody)).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.ExpiresAt != "2030-01-01T00:00:00Z" {
		t.Fatalf("expires_at = %q", resp.ExpiresAt)
	}
	assertExtra := func(key, want string) {
		t.Helper()
		if got := resp.Extra[key]; got != want {
			t.Fatalf("extra[%s] = %q, want %q", key, got, want)
		}
	}
	assertExtra("subscription", "builder id pro")
	assertExtra("plan_type", "Builder Id Pro")
	assertExtra("email", "dev@example.com")
	assertExtra("quota_total", "118")
	assertExtra("quota_used", "32")
	assertExtra("quota_remaining", "86")
	assertExtra("quota_currency", "requests")
	assertExtra("access_token", "access-token")
}

func TestHandleAccountQuotaInvalidBody(t *testing.T) {
	g := &KiroGateway{}
	status, _, _, err := g.HandleRequest(context.Background(), http.MethodPost, "accounts/quota", "", nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("HandleRequest returned error: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
}
