package gateway

import (
	"fmt"
	"net/http"

	"github.com/google/uuid"

	sdk "github.com/DouDOU-start/airgate-sdk"
)

type headerConfig struct {
	KiroVersion   string
	NodeVersion   string
	SystemVersion string
}

func defaultHeaderConfig(ctx sdk.PluginContext) headerConfig {
	cfg := headerConfig{
		KiroVersion:   DefaultKiroVersion,
		NodeVersion:   DefaultNodeVersion,
		SystemVersion: "darwin#24.6.0",
	}
	if ctx != nil && ctx.Config() != nil {
		if v := ctx.Config().GetString("kiro_version"); v != "" {
			cfg.KiroVersion = v
		}
		if v := ctx.Config().GetString("node_version"); v != "" {
			cfg.NodeVersion = v
		}
	}
	return cfg
}

func buildKiroHeaders(account *sdk.Account, region, machineID string, cfg headerConfig) http.Header {
	host := fmt.Sprintf("q.%s.amazonaws.com", region)
	xAmzUA := fmt.Sprintf("aws-sdk-js/1.0.34 KiroIDE-%s-%s", cfg.KiroVersion, machineID)
	ua := fmt.Sprintf(
		"aws-sdk-js/1.0.34 ua/2.1 os/%s lang/js md/nodejs#%s api/codewhispererstreaming#1.0.34 m/E KiroIDE-%s-%s",
		cfg.SystemVersion, cfg.NodeVersion, cfg.KiroVersion, machineID,
	)

	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Connection", "close")
	h.Set("x-amzn-codewhisperer-optout", "true")
	h.Set("x-amzn-kiro-agent-mode", "vibe")
	h.Set("x-amz-user-agent", xAmzUA)
	h.Set("User-Agent", ua)
	h.Set("Host", host)
	h.Set("amz-sdk-invocation-id", uuid.New().String())
	h.Set("amz-sdk-request", "attempt=1; max=3")

	token := account.Credentials["access_token"]
	if account.Type == "api_key" {
		token = account.Credentials["kiro_api_key"]
		h.Set("tokentype", "API_KEY")
	}
	h.Set("Authorization", "Bearer "+token)

	return h
}

func resolveRegion(account *sdk.Account, ctx sdk.PluginContext) string {
	if r := account.Credentials["region"]; r != "" {
		return r
	}
	if ctx != nil && ctx.Config() != nil {
		if r := ctx.Config().GetString("default_region"); r != "" {
			return r
		}
	}
	return DefaultRegion
}
