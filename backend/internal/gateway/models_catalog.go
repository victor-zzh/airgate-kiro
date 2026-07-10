package gateway

import (
	"context"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const (
	catalogRefreshInterval  = 60 * time.Second
	hostMethodModelsCatalog = "models.catalog"
	hostMethodModelsRefresh = "models.refresh"
)

func (g *KiroGateway) runCatalogRefresh(ctx context.Context) {
	g.refreshCatalogOnce(ctx)
	ticker := time.NewTicker(catalogRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.refreshCatalogOnce(ctx)
		}
	}
}

func (g *KiroGateway) refreshCatalogOnce(ctx context.Context) {
	if g.host == nil {
		return
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := g.host.Invoke(fetchCtx, sdk.HostInvokeRequest{
		Method:  hostMethodModelsCatalog,
		Payload: map[string]interface{}{"platform": PluginPlatform},
	})
	if err != nil {
		g.logger.Warn("models_catalog_fetch_failed", sdk.LogFieldError, err)
		return
	}
	if resp == nil {
		return
	}
	if resp.Status == "error" {
		g.logger.Warn("models_catalog_fetch_error", "message", resp.Payload["message"])
		return
	}
	raw, _ := resp.Payload["catalog_json"].(string)
	stats, err := setCatalogOverlayJSON(raw)
	if err != nil {
		g.logger.Warn("models_catalog_parse_failed", sdk.LogFieldError, err)
		return
	}
	g.logger.Debug("models_catalog_applied",
		"models", stats.models,
		"hidden", stats.hidden,
		"pricing", stats.pricing,
	)
	g.maybePushModels(ctx, raw)
}

// maybePushModels 覆盖层内容变化时把当前生效模型清单推给 core(models.refresh),
// 刷新 core 启动时冻结的 modelCache——否则覆盖层新增模型不会出现在
// AI Chat 模型下拉等 core 侧目录消费方里。
//
// 只在应用的 catalog JSON 与上次成功推送的不同时推送;空覆盖层且从未推送过时跳过
// (启动快照本就等于纯内置清单)。仅由 runCatalogRefresh 单 goroutine 调用,无并发。
func (g *KiroGateway) maybePushModels(ctx context.Context, appliedRaw string) {
	if g.catalogPushed && g.pushedCatalogRaw == appliedRaw {
		return
	}
	if !g.catalogPushed && appliedRaw == "" {
		return
	}
	models := allModelInfos()
	entries := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		entries = append(entries, map[string]interface{}{
			"id":                m.ID,
			"name":              m.Name,
			"context_window":    m.ContextWindow,
			"max_output_tokens": m.MaxOutputTokens,
			"capabilities":      m.Capabilities,
			"metadata":          m.Metadata,
		})
	}
	if len(entries) == 0 {
		return
	}
	pushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := g.host.Invoke(pushCtx, sdk.HostInvokeRequest{
		Method:  hostMethodModelsRefresh,
		Payload: map[string]interface{}{"models": entries},
	})
	if err != nil {
		// 老版本 core 无此 method 时会一直失败:只警告,下轮重试
		g.logger.Warn("models_refresh_push_failed", sdk.LogFieldError, err)
		return
	}
	if resp != nil && resp.Status == "error" {
		g.logger.Warn("models_refresh_push_error", "message", resp.Payload["message"])
		return
	}
	g.catalogPushed = true
	g.pushedCatalogRaw = appliedRaw
	g.logger.Info("models_refresh_pushed", "count", len(entries))
}
