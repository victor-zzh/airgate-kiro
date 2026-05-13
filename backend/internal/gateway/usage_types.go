package gateway

import "time"

type quotaInfo struct {
	Used      float64           `json:"used"`
	Total     float64           `json:"total"`
	Remaining float64           `json:"remaining"`
	Currency  string            `json:"currency,omitempty"`
	ExpiresAt string            `json:"expires_at,omitempty"`
	Extra     map[string]string `json:"extra,omitempty"`
}

type accountUsageWindow struct {
	Key          string  `json:"key,omitempty"`
	Label        string  `json:"label"`
	UsedPercent  float64 `json:"used_percent"`
	ResetSeconds int64   `json:"reset_seconds"`
	ResetAt      string  `json:"reset_at,omitempty"`
}

type accountUsageInfo struct {
	UpdatedAt string               `json:"updated_at"`
	Windows   []accountUsageWindow `json:"windows,omitempty"`
}

type accountUsageAccountsResponse struct {
	Accounts map[string]accountUsageInfo `json:"accounts"`
}

func newAccountUsageWindow(key, label string, usedPercent float64, resetAt *time.Time, now time.Time) accountUsageWindow {
	window := accountUsageWindow{
		Key:         key,
		Label:       label,
		UsedPercent: usedPercent,
	}
	if resetAt != nil {
		window.ResetAt = resetAt.UTC().Format(time.RFC3339)
		if resetAt.After(now) {
			window.ResetSeconds = int64(resetAt.Sub(now).Seconds())
		}
	}
	return window
}
