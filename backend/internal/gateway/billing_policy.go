package gateway

import (
	"math"
	"strconv"
	"strings"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const standardKeyBillingHeader = "X-Airgate-Plugin-Openai-Basispoints-Standard-Key-Billing"

// Vendor pricing and vendor-specific group policy belong here. Only generic
// monetary adjustments cross the SDK boundary; Core owns rates and settlement.
func applyProviderBillingPolicy(req *sdk.ForwardRequest, usage *sdk.Usage) {
	if usage == nil || req == nil {
		return
	}
	delete(usage.Metadata, "api_key.billing_service_tier")
	delete(usage.Metadata, "api_key.billing_policy")
	adjustments := &sdk.BillingAdjustments{}
	if cost, ok := configuredImageCharge(req, usage); ok {
		_, path := resolveAPIKeyRoute(req)
		if isImagesRequest(path) {
			adjustments.ChargeOverride = &cost
		} else {
			adjustments.ChargeAddon = &cost
		}
	}
	if basispointsEnabled(req) && strings.EqualFold(req.Headers.Get(standardKeyBillingHeader), "true") && usageMetadataText(usage, "oauth_transport") == "basispoints" {
		if cost, ok := defaultTokenQuote(usage); ok {
			adjustments.APIKeyBaseCost = &cost
			setUsageMetadata(usage, "api_key.billing_service_tier", "default")
			setUsageMetadata(usage, "api_key.billing_policy", "basispoints_standard")
		}
	}
	if adjustments.ChargeOverride == nil && adjustments.ChargeAddon == nil && adjustments.APIKeyBaseCost == nil {
		usage.Billing = nil
		return
	}
	usage.Billing = adjustments
}

func configuredImageCharge(req *sdk.ForwardRequest, usage *sdk.Usage) (float64, bool) {
	if req == nil || usage == nil {
		return 0, false
	}
	count := usageMetricInt(usage, usageMetricImages)
	if count <= 0 {
		return 0, false
	}
	tier, _, ok := imageTierPriceForSize(usageMetadataText(usage, usageAttrImageSize))
	if !ok {
		return 0, false
	}
	raw := strings.TrimSpace(req.Headers.Get("X-Airgate-Plugin-Openai-Image-Price-" + tier))
	price, err := strconv.ParseFloat(raw, 64)
	if err != nil || price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0, false
	}
	cost := float64(count) * price
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0, false
	}
	return cost, true
}
