package gateway

import sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"

//go:generate go run ../../cmd/genmanifest

var PluginVersion = "1.0.0"

const (
	PluginID             = "gateway-kiro"
	PluginName           = "Kiro 网关"
	PluginPlatform       = "kiro"
	PluginMode           = "simple"
	PluginMinCoreVersion = "1.0.0"

	DefaultKiroVersion = "0.11.107"
	DefaultNodeVersion = "22.22.0"
	DefaultRegion      = "us-east-1"
	DefaultKiroCommit  = ""
)

func PluginDependencies() []string {
	return []string{}
}

func buildPluginInfo() sdk.PluginInfo {
	return sdk.PluginInfo{
		ID:          PluginID,
		Name:        PluginName,
		Version:     PluginVersion,
		SDKVersion:  sdk.SDKVersion,
		Description: "Kiro (AWS CodeWhisperer) 反代网关，兼容 Anthropic Messages API",
		Author:      "hopbase",
		Type:        sdk.PluginTypeGateway,
		Capabilities: []sdk.Capability{
			sdk.CapabilityForHostMethod(hostMethodModelsCatalog),
			sdk.CapabilityForHostMethod(hostMethodModelsRefresh),
		},
		FrontendWidgets: []sdk.FrontendWidget{
			{Slot: sdk.SlotAccountCreate, EntryFile: "index.js", Title: "创建账号表单"},
			{Slot: sdk.SlotAccountEdit, EntryFile: "index.js", Title: "编辑账号表单"},
			{Slot: sdk.SlotAccountUsageWindow, EntryFile: "index.js", Title: "账号用量窗口"},
		},
		ConfigSchema: []sdk.ConfigField{
			{Key: "kiro_version", Label: "Kiro IDE 版本号", Type: "string", Default: DefaultKiroVersion},
			{Key: "node_version", Label: "Node.js 版本标识", Type: "string", Default: DefaultNodeVersion},
			{Key: "default_region", Label: "默认 AWS Region", Type: "string", Default: DefaultRegion},
			{Key: "kiro_commit", Label: "Kiro IDE Commit Hash", Type: "string", Default: DefaultKiroCommit},
		},
		Metadata: map[string]string{
			// 对外协议为 Anthropic Messages API：Core 写错误按 Anthropic 形态
			"error_format": "anthropic",
			"account.oauth_plans": `[
				{"key":"free","label":"Free","credential_key":"plan_type","match":"contains","matches":["Free"]},
				{"key":"pro","label":"Pro","credential_key":"plan_type","match":"contains","matches":["Pro"]},
				{"key":"pro_plus","label":"Pro+","credential_key":"plan_type","match":"contains","matches":["Pro+","Pro Plus"]},
				{"key":"power","label":"Power","credential_key":"plan_type","match":"contains","matches":["Power"]}
			]`,
		},
		AccountTypes: []sdk.AccountType{
			{
				Key:         "oauth",
				Label:       "OAuth",
				Description: "通过浏览器 OAuth 或 IdC (BuilderID) 认证",
				Fields: []sdk.CredentialField{
					{Key: "refresh_token", Label: "Refresh Token", Type: "password", Required: true, Placeholder: "从 Kiro IDE 提取"},
					{Key: "access_token", Label: "Access Token", Type: "password", Placeholder: "自动刷新"},
					{Key: "expires_at", Label: "过期时间", Type: "text", Placeholder: "自动填充"},
					{Key: "profile_arn", Label: "Profile ARN", Type: "text", Placeholder: "自动获取"},
					{Key: "client_id", Label: "Client ID (IdC)", Type: "text", Placeholder: "IdC 账号自动填充"},
					{Key: "client_secret", Label: "Client Secret (IdC)", Type: "password", Placeholder: "IdC 账号自动填充"},
					{Key: "region", Label: "AWS Region", Type: "text", Placeholder: DefaultRegion},
					{Key: "machine_id", Label: "Machine ID (64位hex)", Type: "text"},
					{Key: "proxy_url", Label: "代理地址", Type: "text", Placeholder: "http:// 或 socks5://"},
				},
			},
			{
				Key:         "api_key",
				Label:       "Kiro API Key",
				Description: "使用 Kiro API Key (ksk_...) 直接访问",
				Fields: []sdk.CredentialField{
					{Key: "kiro_api_key", Label: "Kiro API Key", Type: "password", Required: true, Placeholder: "ksk_..."},
					{Key: "region", Label: "AWS Region", Type: "text", Placeholder: DefaultRegion},
				},
			},
		},
	}
}

func pluginRoutes() []sdk.RouteDefinition {
	return []sdk.RouteDefinition{
		{Method: "POST", Path: "/v1/messages", Description: "Messages API"},
		{Method: "POST", Path: "/v1/messages/count_tokens", Description: "Token 计数"},
		{Method: "GET", Path: "/v1/models", Description: "模型列表", Metadata: map[string]string{"metadata_only": "true"}},
	}
}
