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

func TestMarkImageRetryUsed(t *testing.T) {
	outcome := sdk.ForwardOutcome{}
	markImageRetryUsed(&outcome)

	if !imageRetryUsed(outcome.Upstream.Headers) {
		t.Fatalf("%s not marked", imageRetryUsedHeader)
	}
}

func TestMarkImageRetryUsedAfterFallbackSkipsRateLimit(t *testing.T) {
	outcome := sdk.ForwardOutcome{Kind: sdk.OutcomeAccountRateLimited}
	markImageRetryUsedAfterFallback(&outcome)

	if imageRetryUsed(outcome.Upstream.Headers) {
		t.Fatalf("%s marked for rate limit", imageRetryUsedHeader)
	}
}

func TestMarkImageRetryUsedAfterFallbackMarksNonRateLimit(t *testing.T) {
	outcome := sdk.ForwardOutcome{Kind: sdk.OutcomeUpstreamTransient}
	markImageRetryUsedAfterFallback(&outcome)

	if !imageRetryUsed(outcome.Upstream.Headers) {
		t.Fatalf("%s not marked for transient failure", imageRetryUsedHeader)
	}
}

func TestImageRetryBudgetDisablesFallbackAttempts(t *testing.T) {
	if got := len(imageSizeAttemptsForRequest("4096x4096")); got != 2 {
		t.Fatalf("imageSizeAttemptsForRequest len = %d, want 2", got)
	}
	if got := len(imageSizeAttemptsForRequestWithBudget("4096x4096", true)); got != 1 {
		t.Fatalf("imageSizeAttemptsForRequestWithBudget len = %d, want 1", got)
	}
}

func TestShouldRetryImageFallbackSkipsRateLimit(t *testing.T) {
	if shouldRetryImageFallback(sdk.ForwardOutcome{Kind: sdk.OutcomeAccountRateLimited}, nil) {
		t.Fatal("rate limited image request should not use 2K fallback")
	}
}
