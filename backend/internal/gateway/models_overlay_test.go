package gateway

import (
	"strings"
	"testing"
)

func withCatalogOverlay(t *testing.T, raw string) {
	t.Helper()
	if _, err := setCatalogOverlayJSON(raw); err != nil {
		t.Fatalf("setCatalogOverlayJSON(%q): %v", raw, err)
	}
	t.Cleanup(resetCatalogOverlay)
}

func TestKiroCatalogOverlay_EmptyOverlaySameAsHardcoded(t *testing.T) {
	withCatalogOverlay(t, "")
	if len(activeKiroModels()) != len(kiroModels) {
		t.Fatalf("空覆盖层模型数 = %d, want %d", len(activeKiroModels()), len(kiroModels))
	}
	for id, want := range pricingRegistry {
		if got := lookupPricing(id); got != want {
			t.Fatalf("空覆盖层 %s pricing = %+v, want %+v", id, got, want)
		}
	}
}

func TestKiroCatalogOverlay_OverrideExistingPrice(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"claude-sonnet-4-6","pricing":{"input":2,"cached_input":0.2,"cache_write_5m":2.5,"cache_write_1h":4,"output":10}}
	]`)

	p := lookupPricing("claude-sonnet-4-6")
	if p.InputPrice != 2 || p.CachedPrice != 0.2 || p.CacheCreationPrice != 2.5 ||
		p.CacheCreation1hPrice != 4 || p.OutputPrice != 10 {
		t.Fatalf("覆盖后 pricing = %+v", p)
	}
}

func TestKiroCatalogOverlay_PartialPricingKeepsSafeDefaults(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"claude-sonnet-4-6","pricing":{"input":2,"cached_input":0,"cache_write_5m":0,"cache_write_1h":0,"output":0}}
	]`)

	p := lookupPricing("claude-sonnet-4-6")
	base := pricingRegistry["claude-sonnet-4-6"]
	if p.InputPrice != 2 {
		t.Fatalf("显式 input 价格未覆盖: %+v", p)
	}
	if p.CachedPrice != base.CachedPrice || p.CacheCreationPrice != base.CacheCreationPrice ||
		p.CacheCreation1hPrice != base.CacheCreation1hPrice || p.OutputPrice != base.OutputPrice {
		t.Fatalf("缺省/0 价格不应覆盖成免费价: got %+v want %+v", p, base)
	}
}

func TestKiroCatalogOverlay_AddModelAndMapping(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"claude-sonnet-5","kiro_id":"claude-sonnet-5.0","name":"Claude Sonnet 5",
	   "context_window":1000000,"max_output_tokens":128000,
	   "pricing":{"input":3,"cached_input":0.3,"cache_write_5m":3.75,"cache_write_1h":6,"output":15}}
	]`)

	kiroID, ctx, err := MapToKiroModel("claude-sonnet-5")
	if err != nil {
		t.Fatalf("MapToKiroModel 新模型: %v", err)
	}
	if kiroID != "claude-sonnet-5.0" || ctx != 1_000_000 {
		t.Fatalf("MapToKiroModel = (%q,%d), want (claude-sonnet-5.0,1000000)", kiroID, ctx)
	}
	if got := MapToAnthropicModel("claude-sonnet-5.0"); got != "claude-sonnet-5" {
		t.Fatalf("MapToAnthropicModel = %q, want claude-sonnet-5", got)
	}
	if !strings.Contains(string(buildModelsResponse()), "claude-sonnet-5") {
		t.Fatal("/v1/models 响应应包含新增模型")
	}
}

func TestKiroCatalogOverlay_DisabledHiddenButBillable(t *testing.T) {
	withCatalogOverlay(t, `[{"id":"claude-haiku-4-5-20251001","enabled":false}]`)

	if got := lookupPricing("claude-haiku-4-5-20251001"); got != pricingRegistry["claude-haiku-4-5-20251001"] {
		t.Fatalf("隐藏模型计费规格不应改变: %+v", got)
	}
	for _, m := range visibleKiroModels() {
		if m.AnthropicID == "claude-haiku-4-5-20251001" {
			t.Fatal("隐藏模型不应出现在 visibleKiroModels")
		}
	}
	if !strings.Contains(MapToAnthropicModel("claude-haiku-4.5"), "claude-haiku-4-5") {
		t.Fatal("隐藏模型仍应保留映射，保证历史请求可计费")
	}
}

func TestKiroCatalogOverlay_MalformedJSONDoesNotReplaceSnapshot(t *testing.T) {
	withCatalogOverlay(t, `[{"id":"claude-sonnet-4-6","pricing":{"input":2,"output":10}}]`)
	before := lookupPricing("claude-sonnet-4-6")
	if _, err := setCatalogOverlayJSON(`{{{`); err == nil {
		t.Fatal("非法 JSON 应返回 error")
	}
	after := lookupPricing("claude-sonnet-4-6")
	if before != after {
		t.Fatalf("解析失败不应替换旧快照: before=%+v after=%+v", before, after)
	}
}
