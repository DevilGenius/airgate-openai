package gateway

import (
	"strings"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const oauthModelEntitlementCooldown = 3 * 24 * time.Hour

// applyOAuthModelEntitlementCooldown turns a high-confidence ChatGPT account
// model-entitlement rejection into the existing account-family rate-limit
// outcome. Core will retry the same model on another account and cool only the
// rejected account/model family instead of degrading the whole OAuth account.
func applyOAuthModelEntitlementCooldown(req *sdk.ForwardRequest, outcome sdk.ForwardOutcome) (sdk.ForwardOutcome, bool) {
	if req == nil || req.Account == nil || outcome.Kind != sdk.OutcomeClientError {
		return outcome, false
	}

	credentials := req.Account.Credentials
	if !isOpenAIOAuthCredentials(credentials) ||
		isOpenAIAgentIdentityCredentials(credentials) ||
		strings.TrimSpace(credentials["api_key"]) != "" {
		return outcome, false
	}
	if !isOAuthModelEntitlementUnavailableText(outcome.Reason, string(outcome.Upstream.Body)) {
		return outcome, false
	}

	outcome.Kind = sdk.OutcomeAccountRateLimited
	outcome.FailoverScope = sdk.FailoverScopeNone
	outcome.RetryAfter = oauthModelEntitlementCooldown
	return outcome, true
}

func isOAuthModelEntitlementUnavailableText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if !strings.Contains(combined, "model") {
		return false
	}
	return strings.Contains(combined, "not supported when using codex with a chatgpt account")
}
