package gateway

import (
	"context"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const (
	invalidEncryptedContentRetryCacheTTL        = 24 * time.Hour
	invalidEncryptedContentRetryCacheMaxEntries = 100_000
)

type encryptedContentRetryRequestStateContextKey struct{}

type encryptedContentRetryRequestState struct {
	cache          *safetyRequestCache
	checkedAt      time.Time
	validHashes    []uint64
	retrySanitized bool
}

func withEncryptedContentRetryRequestState(ctx context.Context, state *encryptedContentRetryRequestState) context.Context {
	if ctx == nil || state == nil {
		return ctx
	}
	return context.WithValue(ctx, encryptedContentRetryRequestStateContextKey{}, state)
}

func encryptedContentRetryRequestStateFromContext(ctx context.Context) *encryptedContentRetryRequestState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(encryptedContentRetryRequestStateContextKey{}).(*encryptedContentRetryRequestState)
	return state
}

func (g *OpenAIGateway) newEncryptedContentRetryRequestState() *encryptedContentRetryRequestState {
	if g == nil {
		return nil
	}
	return &encryptedContentRetryRequestState{
		cache:     &g.encryptedContentRetry,
		checkedAt: time.Now(),
	}
}

func (g *OpenAIGateway) cacheInvalidEncryptedContentRetry(state *encryptedContentRetryRequestState, path string) bool {
	if g == nil || state == nil || !isResponsesRequestPath(path) || len(state.validHashes) == 0 {
		return false
	}
	g.encryptedContentRetry.addHashesWithLimits(
		state.validHashes,
		time.Now(),
		invalidEncryptedContentRetryCacheTTL,
		invalidEncryptedContentRetryCacheMaxEntries,
	)
	return true
}

func isInvalidEncryptedContentOutcome(outcome sdk.ForwardOutcome, err error) bool {
	if outcome.Kind == sdk.OutcomeSuccess && outcome.Upstream.StatusCode < 400 && err == nil {
		return false
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	return isEncryptedContentVerificationError(
		outcome.Reason,
		string(outcome.Upstream.Body),
		errText,
	)
}
