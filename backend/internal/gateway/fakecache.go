package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// promptCacheTracker 模拟 Anthropic prompt cache 行为。
// Kiro 不返回缓存命中信息，但底层 Claude 实际有 prompt cache。
// 通过追踪 system prompt + tools 的 hash，估算缓存命中比例，
// 将输入 Token 拆分为缓存命中部分（按 cache read 计价）和非缓存部分。
type promptCacheTracker struct {
	mu    sync.Mutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	createdAt time.Time
	lastHitAt time.Time
	hitCount  int
}

const (
	cacheTTL      = 5 * time.Minute
	cacheHitRate  = 0.90
	cleanupPeriod = 2 * time.Minute
)

var globalCacheTracker = &promptCacheTracker{
	cache: make(map[string]*cacheEntry),
}

func (t *promptCacheTracker) track(systemPrompt string, toolsJSON string) (isCacheHit bool) {
	key := hashCacheKey(systemPrompt, toolsJSON)
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	t.cleanupLocked(now)

	entry, exists := t.cache[key]
	if exists && now.Sub(entry.lastHitAt) < cacheTTL {
		entry.lastHitAt = now
		entry.hitCount++
		return true
	}

	t.cache[key] = &cacheEntry{
		createdAt: now,
		lastHitAt: now,
		hitCount:  0,
	}
	return false
}

func (t *promptCacheTracker) cleanupLocked(now time.Time) {
	for key, entry := range t.cache {
		if now.Sub(entry.lastHitAt) > cacheTTL*2 {
			delete(t.cache, key)
		}
	}
}

func hashCacheKey(systemPrompt, toolsJSON string) string {
	h := sha256.New()
	h.Write([]byte(systemPrompt))
	h.Write([]byte{0})
	h.Write([]byte(toolsJSON))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// applyCacheToUsage 将缓存模拟结果写入 Usage。
func applyCacheToUsage(usage *sdk.Usage, convCtx *ConvertContext) {
	inputTokens := usageMetricInt(usage, usageMetricInputTokens)
	if usage == nil || convCtx == nil || inputTokens <= 0 {
		return
	}
	cacheHit := globalCacheTracker.track(convCtx.SystemPrompt, convCtx.ToolsJSON)
	nonCached, cached := applyCacheSimulation(inputTokens, cacheHit)
	setUsageTokens(usage, nonCached, usageMetricInt(usage, usageMetricOutputTokens), cached)
}

// applyCacheSimulation 根据缓存命中状态拆分输入 Token。
//
// 缓存命中：将 cacheHitRate 比例的输入标记为缓存输入（按 cache read 0.1x 计价），
// 剩余部分保持为非缓存输入（1x 计价）。
//
// 缓存未命中（首次请求）：全部保持为标准输入，按标准 input 1x 计价。
// 不使用缓存创建 Token 避免首次请求因 1.25x 写入价格反而更贵，
// 且 Core 费用明细面板不显示缓存创建成本行导致金额对不上。
func applyCacheSimulation(inputTokens int, cacheHit bool) (nonCachedInput, cachedInput int) {
	if inputTokens <= 0 {
		return 0, 0
	}

	if cacheHit {
		cachedInput = int(float64(inputTokens) * cacheHitRate)
		nonCachedInput = inputTokens - cachedInput
		return nonCachedInput, cachedInput
	}

	// 首次请求：全部按标准 input 计价
	return inputTokens, 0
}
