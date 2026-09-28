package gateway

import (
	"math"
	"strconv"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const defaultTokenCostMetadata = "openai.billing.default_token_cost"

// Quote only: Core decides whether this base applies to API key quota. Reuse the
// standard calculator, including long-context and cache-write pricing. Never
// change actual service_tier, measured prices, costs or upstream account usage.
func annotateDefaultTokenQuote(usage *sdk.Usage) {
	if usage == nil || usage.Model == "" || usageMetadataText(usage, "oauth_transport") != "basispoints" {
		return
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
		return
	}
	setUsageMetadata(usage, defaultTokenCostMetadata, strconv.FormatFloat(quote, 'g', -1, 64))
}
