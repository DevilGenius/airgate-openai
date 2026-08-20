package gateway

import (
	"errors"
	"testing"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestForwardErrForOutcomeSuppressesClientError(t *testing.T) {
	err := errors.New("boom")
	got := forwardErrForOutcome(sdk.ForwardOutcome{Kind: sdk.OutcomeClientError}, err)
	if got != nil {
		t.Fatalf("expected nil err for client outcome, got %v", got)
	}
}

func TestForwardErrForOutcomeKeepsAccountError(t *testing.T) {
	err := errors.New("boom")
	got := forwardErrForOutcome(sdk.ForwardOutcome{Kind: sdk.OutcomeAccountDead}, err)
	if got != err {
		t.Fatalf("expected original err for account outcome, got %v", got)
	}
}

func TestApplyImageRateLimitPolicy(t *testing.T) {
	outcome := sdk.ForwardOutcome{
		Kind:       sdk.OutcomeAccountRateLimited,
		RetryAfter: 15 * time.Millisecond,
	}

	applyImageRateLimitPolicy(&outcome)

	if outcome.RetryAfter != time.Minute {
		t.Fatalf("RetryAfter = %s, want 1m", outcome.RetryAfter)
	}
}

func TestImageSizeAttemptsForRequestIncludesFallback(t *testing.T) {
	if got := len(imageSizeAttemptsForRequest("4096x4096")); got != 2 {
		t.Fatalf("imageSizeAttemptsForRequest len = %d, want 2", got)
	}
}

func TestShouldRetryImageFallbackSkipsRateLimit(t *testing.T) {
	if shouldRetryImageFallback(sdk.ForwardOutcome{Kind: sdk.OutcomeAccountRateLimited}, nil) {
		t.Fatal("rate limited image request should not use 2K fallback")
	}
}

func TestFillUsageCostGPT56FullPricing(t *testing.T) {
	cases := []struct {
		model              string
		shortInput         float64
		shortCached        float64
		shortCacheCreation float64
		shortOutput        float64
		longInput          float64
		longCached         float64
		longCacheCreation  float64
		longOutput         float64
	}{
		{"gpt-5.6-sol", 5, 0.5, 6.25, 30, 10, 1, 12.5, 45},
		{"gpt-5.6-terra", 2, 0.2, 2.5, 12, 4, 0.4, 5, 18},
		{"gpt-5.6-luna", 1, 0.1, 1.25, 6, 2, 0.2, 2.5, 9},
	}

	for _, tc := range cases {
		t.Run(tc.model+"/short-271999", func(t *testing.T) {
			usage := &sdk.Usage{
				Model:               tc.model,
				InputTokens:         100000,
				CachedInputTokens:   100000,
				CacheCreationTokens: 71999,
				OutputTokens:        1000,
			}
			fillUsageCost(usage)
			assertUsagePricesAndCosts(t, usage, tc.shortInput, tc.shortCached, tc.shortCacheCreation, tc.shortOutput)
		})

		t.Run(tc.model+"/long-272000", func(t *testing.T) {
			usage := &sdk.Usage{
				Model:               tc.model,
				InputTokens:         100000,
				CachedInputTokens:   100000,
				CacheCreationTokens: 72000,
				OutputTokens:        1000,
			}
			fillUsageCost(usage)
			assertUsagePricesAndCosts(t, usage, tc.longInput, tc.longCached, tc.longCacheCreation, tc.longOutput)
		})
	}
}

func TestFillUsageCostKeepsCacheCreationPriceWithoutTokens(t *testing.T) {
	usage := &sdk.Usage{Model: "gpt-5.6-sol", InputTokens: 1}
	fillUsageCost(usage)

	if usage.CacheCreationPrice != 6.25 {
		t.Fatalf("CacheCreationPrice = %v, want 6.25", usage.CacheCreationPrice)
	}
	if usage.CacheCreationCost != 0 {
		t.Fatalf("CacheCreationCost = %v, want 0", usage.CacheCreationCost)
	}
}

func TestGPT56CacheCreationPriceComposesServiceTierAndLongContext(t *testing.T) {
	cases := []struct {
		tier string
		want float64
	}{
		{"priority", 25},
		{"flex", 6.25},
	}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			usage := &sdk.Usage{Model: "gpt-5.6-sol", CacheCreationTokens: 272000}
			setUsageServiceTier(usage, tc.tier)
			fillUsageCost(usage)

			if usage.CacheCreationPrice != tc.want {
				t.Fatalf("CacheCreationPrice = %v, want %v", usage.CacheCreationPrice, tc.want)
			}
		})
	}
}

func assertUsagePricesAndCosts(t *testing.T, usage *sdk.Usage, inputPrice, cachedPrice, cacheCreationPrice, outputPrice float64) {
	t.Helper()

	if !almostEqual(usage.InputPrice, inputPrice, 1e-12) ||
		!almostEqual(usage.CachedInputPrice, cachedPrice, 1e-12) ||
		!almostEqual(usage.CacheCreationPrice, cacheCreationPrice, 1e-12) ||
		!almostEqual(usage.OutputPrice, outputPrice, 1e-12) {
		t.Fatalf("prices = (%v, %v, %v, %v), want (%v, %v, %v, %v)",
			usage.InputPrice, usage.CachedInputPrice, usage.CacheCreationPrice, usage.OutputPrice,
			inputPrice, cachedPrice, cacheCreationPrice, outputPrice,
		)
	}

	wantInputCost := tokenCost(usage.InputTokens, inputPrice)
	wantCachedCost := tokenCost(usage.CachedInputTokens, cachedPrice)
	wantCacheCreationCost := tokenCost(usage.CacheCreationTokens, cacheCreationPrice)
	wantOutputCost := tokenCost(usage.OutputTokens, outputPrice)
	wantAccountCost := wantInputCost + wantCachedCost + wantCacheCreationCost + wantOutputCost
	if !almostEqual(usage.InputCost, wantInputCost, 1e-12) ||
		!almostEqual(usage.CachedInputCost, wantCachedCost, 1e-12) ||
		!almostEqual(usage.CacheCreationCost, wantCacheCreationCost, 1e-12) ||
		!almostEqual(usage.OutputCost, wantOutputCost, 1e-12) ||
		!almostEqual(usage.AccountCost, wantAccountCost, 1e-12) {
		t.Fatalf("costs = (%v, %v, %v, %v, total %v), want (%v, %v, %v, %v, total %v)",
			usage.InputCost, usage.CachedInputCost, usage.CacheCreationCost, usage.OutputCost, usage.AccountCost,
			wantInputCost, wantCachedCost, wantCacheCreationCost, wantOutputCost, wantAccountCost,
		)
	}
}
