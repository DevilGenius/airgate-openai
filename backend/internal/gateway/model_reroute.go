package gateway

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const oauthModelRerouteCode = "model_reroute_required"

// oauthModelRerouteOutcome makes the model fallback visible to Core before the
// selected OAuth account is used. Core can then rebuild the DispatchPlan and
// re-run account policies against the effective upstream model.
func oauthModelRerouteOutcome(req *sdk.ForwardRequest, reqPath string) (sdk.ForwardOutcome, bool) {
	if req == nil || req.Account == nil {
		return sdk.ForwardOutcome{}, false
	}

	credentials := req.Account.Credentials
	if strings.TrimSpace(credentials["access_token"]) == "" || strings.TrimSpace(credentials["api_key"]) != "" {
		return sdk.ForwardOutcome{}, false
	}
	if isAnthropicRequest(req) || isModelsListingRequest(req) || isImagesRequest(reqPath) || isResponsesCompactRequestPath(reqPath) {
		return sdk.ForwardOutcome{}, false
	}
	if !isResponsesRequestPath(reqPath) && !isChatCompletionsRequest(req) {
		return sdk.ForwardOutcome{}, false
	}

	plannedModel := strings.TrimSpace(req.DispatchPlan.UpstreamModel())
	if plannedModel == "" {
		plannedModel = strings.TrimSpace(req.Model)
	}
	targetModel := strings.TrimSpace(resolveEffectiveModel(plannedModel, nil))
	if targetModel == "" || strings.EqualFold(plannedModel, targetModel) {
		return sdk.ForwardOutcome{}, false
	}

	reason := fmt.Sprintf(
		"OAuth Codex model %q resolves to %q; rerouting before account policy evaluation",
		plannedModel,
		targetModel,
	)
	body := openAIErrorJSON("invalid_request_error", oauthModelRerouteCode, reason)
	return sdk.ForwardOutcome{
		Kind:               sdk.OutcomeClientError,
		FailoverScope:      sdk.FailoverScopeModelReroute,
		RerouteClientModel: targetModel,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Headers: http.Header{
				"Content-Type":   []string{"application/json"},
				"Content-Length": []string{strconv.Itoa(len(body))},
			},
			Body: body,
		},
		Reason: reason,
	}, true
}
