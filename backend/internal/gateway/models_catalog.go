package gateway

import (
	"context"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const (
	catalogRefreshInterval  = 60 * time.Second
	hostMethodModelsCatalog = "models.catalog"
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
}
