package gateway

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestDefaultTokenQuotePreservesActualBilling(t *testing.T) {
	for _, model := range []string{"gpt-5.6-sol", "gpt-6-astra"} {
		for _, tier := range []string{"priority", "flex", "default", "auto", "future-tier", ""} {
			for _, input := range []int{100, 300000} {
				t.Run(fmt.Sprintf("%s/%s/%d", model, tier, input), func(t *testing.T) {
					actual := newTokenUsage(model, tier, input, 50, 20, 10, 5, 0)
					setUsageMetadata(actual, "oauth_transport", "basispoints")
					fillUsageCost(actual)
					before := snapshotUnifiedBill(t, actual)
					standard := newTokenUsage(model, "default", input, 50, 20, 10, 5, 0)
					fillUsageCost(standard)
					outcome := applyForwardOutcomePolicies(nil, bpsRequest(), sdk.ForwardOutcome{Kind: sdk.OutcomeSuccess, Usage: actual})
					quote, err := strconv.ParseFloat(outcome.Usage.Metadata[defaultTokenCostMetadata], 64)
					if err != nil || math.Abs(quote-standard.AccountCost) > 1e-12 {
						t.Fatalf("quote=%g want=%g err=%v", quote, standard.AccountCost, err)
					}
					if !reflect.DeepEqual(before, snapshotUnifiedBill(t, actual)) {
						t.Fatal("actual model/tier/prices/costs changed")
					}
				})
			}
		}
	}
}

func TestDefaultTokenQuoteOnlyForBAS(t *testing.T) {
	for _, tc := range []struct {
		tier, transport string
		want            bool
	}{{"default", "basispoints", true}, {"priority", "native", false}, {"flex", "", false}, {"priority", "basispoints", true}, {"flex", "basispoints", true}, {"future-tier", "basispoints", true}, {"", "basispoints", true}} {
		usage := newTokenUsage("gpt-5.6-sol", tc.tier, 100, 50, 20, 10, 0, 0)
		fillUsageCost(usage)
		setUsageMetadata(usage, "oauth_transport", tc.transport)
		outcome := applyForwardOutcomePolicies(nil, bpsRequest(), sdk.ForwardOutcome{Kind: sdk.OutcomeStreamAborted, Usage: usage})
		_, exists := outcome.Usage.Metadata[defaultTokenCostMetadata]
		if exists != tc.want {
			t.Fatalf("%+v quote exists=%v", tc, exists)
		}
	}
}
