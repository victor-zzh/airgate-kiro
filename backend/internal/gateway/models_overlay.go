package gateway

import (
	"encoding/json"
	"strings"
	"sync/atomic"
)

type catalogOverlayStats struct {
	models  int
	hidden  int
	pricing int
}

type catalogOverlay struct {
	models  []kiroModelSpec
	visible []kiroModelSpec
	pricing map[string]pricingSpec
}

var overlayStore atomic.Pointer[catalogOverlay]

func activeKiroModels() []kiroModelSpec {
	if ov := overlayStore.Load(); ov != nil {
		return ov.models
	}
	return kiroModels
}

func visibleKiroModels() []kiroModelSpec {
	if ov := overlayStore.Load(); ov != nil {
		return ov.visible
	}
	return kiroModels
}

func activePricingRegistry() map[string]pricingSpec {
	if ov := overlayStore.Load(); ov != nil {
		return ov.pricing
	}
	return pricingRegistry
}

func resetCatalogOverlay() {
	overlayStore.Store(nil)
}

func setCatalogOverlayJSON(raw string) (catalogOverlayStats, error) {
	ov, err := parseCatalogOverlay(raw)
	if err != nil {
		return catalogOverlayStats{}, err
	}
	overlayStore.Store(ov)
	return catalogOverlayStats{
		models:  len(ov.models),
		hidden:  len(ov.models) - len(ov.visible),
		pricing: len(ov.pricing),
	}, nil
}

type overlayPricing struct {
	Input        float64 `json:"input"`
	CachedInput  float64 `json:"cached_input"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	Output       float64 `json:"output"`
}

type overlayEntry struct {
	ID            string          `json:"id"`
	KiroID        string          `json:"kiro_id,omitempty"`
	UpstreamID    string          `json:"upstream_id,omitempty"`
	Name          string          `json:"name,omitempty"`
	ContextWindow int             `json:"context_window,omitempty"`
	MaxOutput     int             `json:"max_output_tokens,omitempty"`
	Enabled       *bool           `json:"enabled,omitempty"`
	Pricing       *overlayPricing `json:"pricing,omitempty"`
}

func (e overlayEntry) disabled() bool { return e.Enabled != nil && !*e.Enabled }

func parseCatalogOverlay(raw string) (*catalogOverlay, error) {
	models := cloneKiroModels(kiroModels)
	pricing := clonePricingRegistry(pricingRegistry)
	hidden := map[string]bool{}

	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return visibleOverlay(models, pricing, hidden), nil
	}

	var entries []overlayEntry
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return nil, err
	}

	index := make(map[string]int, len(models)*2)
	for i, m := range models {
		index[strings.ToLower(m.AnthropicID)] = i
		index[strings.ToLower(m.KiroID)] = i
	}

	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		i, ok := index[key]
		if !ok && e.KiroID != "" {
			i, ok = index[strings.ToLower(strings.TrimSpace(e.KiroID))]
		}
		if !ok && e.UpstreamID != "" {
			i, ok = index[strings.ToLower(strings.TrimSpace(e.UpstreamID))]
		}

		var m kiroModelSpec
		if ok {
			m = models[i]
		} else {
			m = kiroModelSpec{
				KiroID:        firstNonEmpty(e.KiroID, e.UpstreamID, id),
				AnthropicID:   id,
				Name:          firstNonEmpty(e.Name, id),
				ContextWindow: 200_000,
				MaxOutput:     64_000,
			}
		}
		m = applyOverlayToModel(m, e)
		if ok {
			models[i] = m
			index[strings.ToLower(m.AnthropicID)] = i
			index[strings.ToLower(m.KiroID)] = i
		} else {
			index[strings.ToLower(m.AnthropicID)] = len(models)
			index[strings.ToLower(m.KiroID)] = len(models)
			models = append(models, m)
		}

		basePrice, priceOK := pricing[m.AnthropicID]
		if !priceOK {
			basePrice = inferPricing(m.AnthropicID, pricing)
		}
		if e.Pricing != nil {
			basePrice = applyOverlayToPricing(basePrice, *e.Pricing)
		}
		pricing[m.AnthropicID] = basePrice

		if e.disabled() {
			hidden[m.AnthropicID] = true
		} else {
			delete(hidden, m.AnthropicID)
		}
	}

	return visibleOverlay(models, pricing, hidden), nil
}

func visibleOverlay(models []kiroModelSpec, pricing map[string]pricingSpec, hidden map[string]bool) *catalogOverlay {
	visible := make([]kiroModelSpec, 0, len(models))
	for _, m := range models {
		if hidden[m.AnthropicID] {
			continue
		}
		visible = append(visible, m)
	}
	return &catalogOverlay{models: models, visible: visible, pricing: pricing}
}

func cloneKiroModels(src []kiroModelSpec) []kiroModelSpec {
	out := make([]kiroModelSpec, len(src))
	copy(out, src)
	return out
}

func clonePricingRegistry(src map[string]pricingSpec) map[string]pricingSpec {
	out := make(map[string]pricingSpec, len(src))
	for id, p := range src {
		out[id] = p
	}
	return out
}

func applyOverlayToModel(base kiroModelSpec, e overlayEntry) kiroModelSpec {
	if e.KiroID != "" {
		base.KiroID = strings.TrimSpace(e.KiroID)
	}
	if e.UpstreamID != "" {
		base.KiroID = strings.TrimSpace(e.UpstreamID)
	}
	if e.ID != "" {
		base.AnthropicID = strings.TrimSpace(e.ID)
	}
	if e.Name != "" {
		base.Name = e.Name
	}
	if e.ContextWindow > 0 {
		base.ContextWindow = e.ContextWindow
	}
	if e.MaxOutput > 0 {
		base.MaxOutput = e.MaxOutput
	}
	return base
}

func applyOverlayToPricing(base pricingSpec, p overlayPricing) pricingSpec {
	if p.Input > 0 {
		base.InputPrice = p.Input
	}
	if p.CachedInput > 0 {
		base.CachedPrice = p.CachedInput
	}
	if p.CacheWrite5m > 0 {
		base.CacheCreationPrice = p.CacheWrite5m
	}
	if p.CacheWrite1h > 0 {
		base.CacheCreation1hPrice = p.CacheWrite1h
	}
	if p.Output > 0 {
		base.OutputPrice = p.Output
	}
	return base
}

func inferPricing(id string, reg map[string]pricingSpec) pricingSpec {
	lower := strings.ToLower(id)
	switch {
	case strings.Contains(lower, "opus"):
		if p, ok := reg["claude-opus-4-7"]; ok {
			return p
		}
	case strings.Contains(lower, "haiku"):
		if p, ok := reg["claude-haiku-4-5-20251001"]; ok {
			return p
		}
	case strings.Contains(lower, "sonnet"):
		if p, ok := reg["claude-sonnet-4-6"]; ok {
			return p
		}
	}
	return fallbackPricing
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
