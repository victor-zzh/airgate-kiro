package gateway

import (
	"fmt"
	"net/http"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const (
	usageCurrencyUSD = "USD"

	usageAttrModel = "model"

	usageMetricInputTokens       = "input_tokens"
	usageMetricCachedInputTokens = "cached_input_tokens"
	usageMetricOutputTokens      = "output_tokens"
	usageMetricTotalTokens       = "total_tokens"

	usageCostInput       = "input_tokens"
	usageCostCachedInput = "cached_input_tokens"
	usageCostOutput      = "output_tokens"
)

func successOutcome(statusCode int, body []byte, headers http.Header, usage *sdk.Usage) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{
			StatusCode: statusCode,
			Headers:    headers,
			Body:       body,
		},
		Usage: usage,
	}
}

func failureOutcome(statusCode int, body []byte, headers http.Header, message string, retryAfter time.Duration) sdk.ForwardOutcome {
	kind := classifyHTTPFailure(statusCode, message)
	reason := message
	if reason != "" {
		reason = fmt.Sprintf("HTTP %d: %s", statusCode, message)
	}
	return sdk.ForwardOutcome{
		Kind: kind,
		Upstream: sdk.UpstreamResponse{
			StatusCode: statusCode,
			Headers:    headers,
			Body:       body,
		},
		Reason:     reason,
		RetryAfter: retryAfter,
	}
}

func transientOutcome(reason string) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeUpstreamTransient,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusBadGateway},
		Reason:   reason,
	}
}

func accountDeadOutcome(reason string) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeAccountDead,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusUnauthorized},
		Reason:   reason,
	}
}

func streamAbortedOutcome(statusCode int, reason string, usage *sdk.Usage) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeStreamAborted,
		Upstream: sdk.UpstreamResponse{
			StatusCode: statusCode,
		},
		Reason: reason,
		Usage:  usage,
	}
}

func newTokenUsage(modelID string, inputTokens, outputTokens, cachedInputTokens int, firstTokenMs int64) *sdk.Usage {
	usage := &sdk.Usage{
		Model:        modelID,
		Currency:     usageCurrencyUSD,
		FirstTokenMs: firstTokenMs,
	}
	setUsageModelAttribute(usage, modelID)
	setUsageTokens(usage, inputTokens, outputTokens, cachedInputTokens)
	return usage
}

func setUsageModelAttribute(usage *sdk.Usage, modelID string) {
	if usage == nil || modelID == "" {
		return
	}
	setUsageAttribute(usage, sdk.UsageAttribute{
		Key:   usageAttrModel,
		Label: "模型",
		Kind:  "model",
		Value: modelID,
	})
}

func setUsageTokens(usage *sdk.Usage, inputTokens, outputTokens, cachedInputTokens int) {
	if usage == nil {
		return
	}
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricInputTokens,
		Label: "输入 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(inputTokens),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricCachedInputTokens,
		Label: "缓存输入 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(cachedInputTokens),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricOutputTokens,
		Label: "输出 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(outputTokens),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricTotalTokens,
		Label: "总 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(inputTokens + cachedInputTokens + outputTokens),
	})
}

func setUsageInputTokens(usage *sdk.Usage, inputTokens int) {
	setUsageTokens(
		usage,
		inputTokens,
		usageMetricInt(usage, usageMetricOutputTokens),
		usageMetricInt(usage, usageMetricCachedInputTokens),
	)
}

func addUsageOutputTokens(usage *sdk.Usage, delta int) {
	if delta <= 0 {
		return
	}
	setUsageTokens(
		usage,
		usageMetricInt(usage, usageMetricInputTokens),
		usageMetricInt(usage, usageMetricOutputTokens)+delta,
		usageMetricInt(usage, usageMetricCachedInputTokens),
	)
}

func usageMetricInt(usage *sdk.Usage, key string) int {
	return int(usageMetricValue(usage, key))
}

func usageMetricValue(usage *sdk.Usage, key string) float64 {
	if usage == nil {
		return 0
	}
	for _, metric := range usage.Metrics {
		if metric.Key == key {
			return metric.Value
		}
	}
	return 0
}

func setUsageAttribute(usage *sdk.Usage, attr sdk.UsageAttribute) {
	if usage == nil {
		return
	}
	for i := range usage.Attributes {
		if usage.Attributes[i].Key == attr.Key {
			usage.Attributes[i] = attr
			return
		}
	}
	usage.Attributes = append(usage.Attributes, attr)
}

func setUsageMetric(usage *sdk.Usage, metric sdk.UsageMetric) {
	if usage == nil {
		return
	}
	for i := range usage.Metrics {
		if usage.Metrics[i].Key == metric.Key {
			usage.Metrics[i] = metric
			return
		}
	}
	usage.Metrics = append(usage.Metrics, metric)
}

func setUsageCostDetail(usage *sdk.Usage, detail sdk.UsageCostDetail) {
	if usage == nil {
		return
	}
	if detail.AccountCost <= 0 {
		removeUsageCostDetail(usage, detail.Key)
		return
	}
	for i := range usage.CostDetails {
		if usage.CostDetails[i].Key == detail.Key {
			usage.CostDetails[i] = detail
			recomputeUsageAccountCost(usage)
			return
		}
	}
	usage.CostDetails = append(usage.CostDetails, detail)
	recomputeUsageAccountCost(usage)
}

func removeUsageCostDetail(usage *sdk.Usage, key string) {
	if usage == nil {
		return
	}
	for i := range usage.CostDetails {
		if usage.CostDetails[i].Key == key {
			usage.CostDetails = append(usage.CostDetails[:i], usage.CostDetails[i+1:]...)
			recomputeUsageAccountCost(usage)
			return
		}
	}
}

func recomputeUsageAccountCost(usage *sdk.Usage) {
	if usage == nil {
		return
	}
	var total float64
	for _, detail := range usage.CostDetails {
		total += detail.AccountCost
	}
	usage.AccountCost = total
	usage.Currency = usageCurrencyUSD
	if total > 0 {
		usage.Summary = fmt.Sprintf("标准成本 $%.6f", total)
	}
}
