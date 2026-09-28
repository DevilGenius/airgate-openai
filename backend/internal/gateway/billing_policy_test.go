package gateway

import (
	"fmt"
	"math"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestProviderImageBillingQuotes(t *testing.T) {
	for _, tc := range []struct{ size, tier string }{{"1024x1024", "1k"}, {"1672x941", "2k"}, {"2560x1440", "2k"}, {"3840x2160", "4k"}} {
		for _, path := range []string{"/v1/images/generations", "/v1/images/edits", "/v1/responses"} {
			t.Run(tc.size+path, func(t *testing.T) {
				req := bpsRequest()
				req.Headers.Set("X-Forwarded-Path", path)
				req.Headers.Set("X-Airgate-Plugin-Openai-Image-Price-"+tc.tier, "0.08")
				usage := newTokenUsage("gpt-5.6-sol", "priority", 100, 50, 20, 10, 0, 0)
				fillUsageCost(usage)
				setUsageImageSize(usage, tc.size)
				setUsageMetadataInt(usage, usageMetricImages, 2)
				before := usage.AccountCost
				applyProviderBillingPolicy(req, usage)
				if usage.Billing == nil {
					t.Fatal("missing image quote")
				}
				quote := usage.Billing.ChargeAddon
				if isImagesRequest(path) {
					quote = usage.Billing.ChargeOverride
					if usage.Billing.ChargeAddon != nil {
						t.Fatal("image submit should replace charge")
					}
				} else if usage.Billing.ChargeOverride != nil {
					t.Fatal("responses should retain token charge")
				}
				if quote == nil || math.Abs(*quote-0.16) > 1e-9 || usage.AccountCost != before || usageServiceTier(usage) != "priority" {
					t.Fatalf("quote=%+v actual=%g", usage.Billing, usage.AccountCost)
				}
			})
		}
	}
}

func TestProviderBillingQuoteValidation(t *testing.T) {
	for _, price := range []string{"", "-1", "bad", "NaN", "Inf", "1e309", "0"} {
		req := bpsRequest()
		req.Headers.Set("X-Forwarded-Path", "/v1/responses")
		req.Headers.Set("X-Airgate-Plugin-Openai-Image-Price-1k", price)
		usage := &sdk.Usage{Metadata: map[string]string{"openai.image.size": "1024x1024", "openai.image.count": "2"}}
		applyProviderBillingPolicy(req, usage)
		if price == "0" {
			if usage.Billing == nil || usage.Billing.ChargeAddon == nil || *usage.Billing.ChargeAddon != 0 {
				t.Fatal("explicit free image quote lost")
			}
		} else if usage.Billing != nil {
			t.Errorf("invalid price %q was accepted", price)
		}
	}
	for _, size := range []string{"bad", "0x1024", "1024x0", "1024xbad"} {
		req := bpsRequest()
		req.Headers.Set("X-Airgate-Plugin-Openai-Image-Price-1k", "1")
		usage := &sdk.Usage{Metadata: map[string]string{"openai.image.size": size, "openai.image.count": "1"}}
		if cost, ok := configuredImageCharge(req, usage); ok || cost != 0 {
			t.Errorf("invalid size %s accepted", size)
		}
	}
}

func TestProviderBASKeyPolicyOnlyWhenConfigured(t *testing.T) {
	for _, tier := range []string{"priority", "fast", "flex", "default", "", "future-tier"} {
		t.Run(fmt.Sprintf("tier=%s", tier), func(t *testing.T) {
			req := bpsRequest()
			usage := newTokenUsage("gpt-5.6-sol", tier, 100, 50, 20, 10, 0, 0)
			fillUsageCost(usage)
			setUsageMetadata(usage, "oauth_transport", "basispoints")
			applyProviderBillingPolicy(req, usage)
			if usage.Billing != nil {
				t.Fatal("default policy changed billing")
			}
			req.Headers.Set(standardKeyBillingHeader, "true")
			applyProviderBillingPolicy(req, usage)
			if usage.Billing == nil || usage.Billing.APIKeyBaseCost == nil || usage.Billing.ChargeOverride != nil {
				t.Fatal("policy must override only API key base")
			}
			req.Headers.Set(basispointsSettingHeader, "false")
			applyProviderBillingPolicy(req, usage)
			if usage.Billing != nil || usage.Metadata["api_key.billing_policy"] != "" {
				t.Fatal("disabled policy left stale quote")
			}
		})
	}
}
