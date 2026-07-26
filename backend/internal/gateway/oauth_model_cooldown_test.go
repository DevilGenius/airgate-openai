package gateway

import (
	"net/http"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestApplyOAuthModelEntitlementCooldown(t *testing.T) {
	const message = "The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."
	baseOutcome := sdk.ForwardOutcome{
		Kind:          sdk.OutcomeClientError,
		FailoverScope: sdk.FailoverScopeDispatchCandidate,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Body:       []byte(`{"error":{"message":"` + message + `"}}`),
		},
		Reason: message,
	}

	tests := []struct {
		name        string
		credentials map[string]string
		outcome     sdk.ForwardOutcome
		wantApplied bool
	}{
		{
			name:        "chatgpt oauth entitlement rejection",
			credentials: map[string]string{"access_token": "oauth-token"},
			outcome:     baseOutcome,
			wantApplied: true,
		},
		{
			name:        "api key account remains client error",
			credentials: map[string]string{"api_key": "sk-test"},
			outcome:     baseOutcome,
		},
		{
			name:        "mixed credentials remain api key routed",
			credentials: map[string]string{"access_token": "oauth-token", "api_key": "sk-test"},
			outcome:     baseOutcome,
		},
		{
			name: "agent identity remains separate",
			credentials: map[string]string{
				"agent_runtime_id":  "runtime-id",
				"agent_private_key": "private-key",
			},
			outcome: baseOutcome,
		},
		{
			name:        "generic missing model remains client error",
			credentials: map[string]string{"access_token": "oauth-token"},
			outcome: sdk.ForwardOutcome{
				Kind:          sdk.OutcomeClientError,
				FailoverScope: sdk.FailoverScopeDispatchCandidate,
				Reason:        "The model does not exist.",
			},
		},
		{
			name:        "successful response remains unchanged",
			credentials: map[string]string{"access_token": "oauth-token"},
			outcome:     sdk.ForwardOutcome{Kind: sdk.OutcomeSuccess},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := &sdk.ForwardRequest{Account: &sdk.Account{Credentials: test.credentials}}
			got, applied := applyOAuthModelEntitlementCooldown(req, test.outcome)
			if applied != test.wantApplied {
				t.Fatalf("applied = %v, want %v; outcome = %+v", applied, test.wantApplied, got)
			}
			if !test.wantApplied {
				if got.Kind != test.outcome.Kind || got.FailoverScope != test.outcome.FailoverScope || got.RetryAfter != test.outcome.RetryAfter {
					t.Fatalf("outcome changed unexpectedly: got=%+v want=%+v", got, test.outcome)
				}
				return
			}
			if got.Kind != sdk.OutcomeAccountRateLimited {
				t.Fatalf("Kind = %v, want OutcomeAccountRateLimited", got.Kind)
			}
			if got.FailoverScope != sdk.FailoverScopeNone {
				t.Fatalf("FailoverScope = %q, want none", got.FailoverScope)
			}
			if got.RetryAfter != oauthModelEntitlementCooldown {
				t.Fatalf("RetryAfter = %v, want %v", got.RetryAfter, oauthModelEntitlementCooldown)
			}
			if got.Upstream.StatusCode != test.outcome.Upstream.StatusCode || string(got.Upstream.Body) != string(test.outcome.Upstream.Body) {
				t.Fatalf("upstream response changed: got=%+v want=%+v", got.Upstream, test.outcome.Upstream)
			}
		})
	}
}
