package gateway

import (
	"math"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// Build a private counterfactual quote using the same vendor pricing engine.
// It never changes actual service tier, measured prices, costs or upstream usage.
func defaultTokenQuote(usage *sdk.Usage) (float64, bool) {
	if usage == nil || usage.Model == "" {
		return 0, false
	}
	standard := *usage
	standard.Metadata = make(map[string]string, len(usage.Metadata))
	for key, value := range usage.Metadata {
		standard.Metadata[key] = value
	}
	setUsageServiceTier(&standard, "default")
	fillUsageCost(&standard)
	quote := standard.InputCost + standard.OutputCost + standard.CachedInputCost + standard.CacheCreationCost
	if quote < 0 || math.IsNaN(quote) || math.IsInf(quote, 0) {
		return 0, false
	}
	return quote, true
}
