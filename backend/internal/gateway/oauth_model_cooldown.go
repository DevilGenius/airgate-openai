package gateway

import (
	"strings"
	"sync"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

var oauthModelEntitlementBackoffSchedule = [...]time.Duration{
	time.Minute,
	3 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
}

type oauthModelEntitlementBackoffKey struct {
	accountID int64
	model     string
}

type oauthModelEntitlementBackoffTracker struct {
	mu       sync.Mutex
	failures map[oauthModelEntitlementBackoffKey]int
}

var oauthModelEntitlementBackoff oauthModelEntitlementBackoffTracker

// applyOAuthModelEntitlementCooldown turns a high-confidence ChatGPT account
// model-entitlement rejection into the existing account-family rate-limit
// outcome. Core will retry the same model on another account and cool only the
// rejected account/model family instead of degrading the whole OAuth account.
func applyOAuthModelEntitlementCooldown(req *sdk.ForwardRequest, outcome sdk.ForwardOutcome) (sdk.ForwardOutcome, bool) {
	return oauthModelEntitlementBackoff.apply(req, outcome)
}

func (t *oauthModelEntitlementBackoffTracker) apply(req *sdk.ForwardRequest, outcome sdk.ForwardOutcome) (sdk.ForwardOutcome, bool) {
	key, ok := oauthModelEntitlementTrackerKey(req)
	if !ok {
		return outcome, false
	}

	credentials := req.Account.Credentials
	if !isOpenAIOAuthCredentials(credentials) ||
		isOpenAIAgentIdentityCredentials(credentials) ||
		strings.TrimSpace(credentials["api_key"]) != "" {
		return outcome, false
	}
	if outcome.Kind == sdk.OutcomeSuccess {
		t.reset(key)
		return outcome, false
	}
	if outcome.Kind != sdk.OutcomeClientError {
		return outcome, false
	}
	if !isOAuthModelEntitlementUnavailableText(outcome.Reason, string(outcome.Upstream.Body)) {
		return outcome, false
	}

	outcome.Kind = sdk.OutcomeAccountRateLimited
	outcome.FailoverScope = sdk.FailoverScopeNone
	outcome.RetryAfter = t.next(key)
	return outcome, true
}

func oauthModelEntitlementTrackerKey(req *sdk.ForwardRequest) (oauthModelEntitlementBackoffKey, bool) {
	if req == nil || req.Account == nil {
		return oauthModelEntitlementBackoffKey{}, false
	}
	model := firstNonEmptyString(
		req.DispatchPlan.SchedulingModel,
		req.DispatchPlan.UpstreamModel(),
		req.Model,
	)
	return oauthModelEntitlementBackoffKey{
		accountID: req.Account.ID,
		model:     strings.ToLower(strings.TrimSpace(model)),
	}, true
}

func (t *oauthModelEntitlementBackoffTracker) next(key oauthModelEntitlementBackoffKey) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.failures == nil {
		t.failures = make(map[oauthModelEntitlementBackoffKey]int)
	}
	step := t.failures[key]
	if step < len(oauthModelEntitlementBackoffSchedule) {
		t.failures[key] = step + 1
	}
	if step >= len(oauthModelEntitlementBackoffSchedule) {
		step = len(oauthModelEntitlementBackoffSchedule) - 1
	}
	return oauthModelEntitlementBackoffSchedule[step]
}

func (t *oauthModelEntitlementBackoffTracker) reset(key oauthModelEntitlementBackoffKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, key)
}

func isOAuthModelEntitlementUnavailableText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if !strings.Contains(combined, "model") {
		return false
	}
	return strings.Contains(combined, "not supported when using codex with a chatgpt account")
}
