package gateway

import (
	"log/slog"
	"strings"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// applyForwardOutcomePolicies is the single post-forward policy boundary.
// Transport and response handlers return protocol-level outcomes; account and
// routing policies are normalized here before Core observes the result.
func applyForwardOutcomePolicies(logger *slog.Logger, req *sdk.ForwardRequest, outcome sdk.ForwardOutcome) sdk.ForwardOutcome {
	// BPS entitlement and quota are independent from the native OAuth channel.
	if strings.HasPrefix(outcome.Reason, "basispoints_") || usageMetadataText(outcome.Usage, "oauth_transport") == "basispoints" {
		return outcome
	}
	cooledOutcome, applied := applyOAuthModelEntitlementCooldown(req, outcome)
	if !applied {
		return outcome
	}

	if logger != nil {
		accountID := int64(0)
		model := ""
		if req != nil {
			model = firstNonEmptyString(req.DispatchPlan.UpstreamModel(), req.Model)
			if req.Account != nil {
				accountID = req.Account.ID
			}
		}
		logger.Warn("oauth_model_entitlement_family_cooldown",
			sdk.LogFieldAccountID, accountID,
			sdk.LogFieldModel, model,
			"retry_after", cooledOutcome.RetryAfter,
			sdk.LogFieldReason, cooledOutcome.Reason,
		)
	}
	return cooledOutcome
}
