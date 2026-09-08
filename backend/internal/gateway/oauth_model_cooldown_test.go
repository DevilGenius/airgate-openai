package gateway

import (
	"net/http"
	"testing"
	"time"

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
	const accessMessage = "The model `gpt-5.5` does not exist or you do not have access to it."
	accessOutcome := failureOutcome(400, openAIErrorJSON("invalid_request_error", "model_not_found", accessMessage), nil, accessMessage, 0)
	bodyOnlyAccessOutcome := accessOutcome
	bodyOnlyAccessOutcome.Reason = ""
	notFoundAccessOutcome := accessOutcome
	notFoundAccessOutcome.Upstream.StatusCode = 404

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
			name:        "chatgpt oauth model access rejection HTTP 400",
			credentials: map[string]string{"access_token": "oauth-token"},
			outcome:     accessOutcome,
			wantApplied: true,
		},
		{
			name:        "model access rejection in upstream body only",
			credentials: map[string]string{"access_token": "oauth-token"},
			outcome:     bodyOnlyAccessOutcome,
			wantApplied: true,
		},
		{
			name:        "model access rejection HTTP 404 remains unchanged",
			credentials: map[string]string{"access_token": "oauth-token"},
			outcome:     notFoundAccessOutcome,
		},
		{
			name:        "api key model access rejection remains client error",
			credentials: map[string]string{"api_key": "sk-test"},
			outcome:     accessOutcome,
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
			outcome:     failureOutcome(400, nil, nil, "The model does not exist.", 0),
		},
		{
			name:        "successful response remains unchanged",
			credentials: map[string]string{"access_token": "oauth-token"},
			outcome:     sdk.ForwardOutcome{Kind: sdk.OutcomeSuccess},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := &oauthModelEntitlementBackoffTracker{}
			req := &sdk.ForwardRequest{
				Account:      &sdk.Account{ID: 1, Credentials: test.credentials},
				DispatchPlan: sdk.DispatchPlan{SchedulingModel: "gpt-5.6-sol"},
			}
			got, applied := tracker.apply(req, test.outcome)
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
			if !got.Kind.ShouldFailover() || !got.Kind.IsAccountFault() {
				t.Fatal("expected account failover excluding the rejected account")
			}
			if got.FailoverScope != sdk.FailoverScopeNone {
				t.Fatalf("FailoverScope = %q, want none", got.FailoverScope)
			}
			if got.RetryAfter != oauthModelEntitlementBackoffSchedule[0] {
				t.Fatalf("RetryAfter = %v, want %v", got.RetryAfter, oauthModelEntitlementBackoffSchedule[0])
			}
			if got.Upstream.StatusCode != test.outcome.Upstream.StatusCode || string(got.Upstream.Body) != string(test.outcome.Upstream.Body) {
				t.Fatalf("upstream response changed: got=%+v want=%+v", got.Upstream, test.outcome.Upstream)
			}
		})
	}
}

func TestOAuthModelEntitlementBackoffSequenceAndSuccessReset(t *testing.T) {
	const message = "The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."
	tracker := &oauthModelEntitlementBackoffTracker{}
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{
			ID:          1,
			Credentials: map[string]string{"access_token": "oauth-token"},
		},
		DispatchPlan: sdk.DispatchPlan{SchedulingModel: "gpt-5.6-sol"},
	}
	unsupported := sdk.ForwardOutcome{
		Kind:   sdk.OutcomeClientError,
		Reason: message,
	}
	const accessMessage = "The model `gpt-5.6-sol` does not exist or you do not have access to it."
	accessOutcome := failureOutcome(400, nil, nil, accessMessage, 0)

	wantSequence := append(oauthModelEntitlementBackoffSchedule[:], 2*time.Hour)
	for index, want := range wantSequence {
		outcome := unsupported
		if index%2 == 1 {
			outcome = accessOutcome
		}
		got, applied := tracker.apply(req, outcome)
		if !applied {
			t.Fatalf("attempt %d was not converted to cooldown", index+1)
		}
		if got.RetryAfter != want {
			t.Fatalf("attempt %d RetryAfter = %v, want %v", index+1, got.RetryAfter, want)
		}
	}

	if got, applied := tracker.apply(req, sdk.ForwardOutcome{Kind: sdk.OutcomeSuccess}); applied || got.Kind != sdk.OutcomeSuccess {
		t.Fatalf("success outcome changed unexpectedly: got=%+v applied=%v", got, applied)
	}
	got, applied := tracker.apply(req, accessOutcome)
	if !applied || got.RetryAfter != time.Minute {
		t.Fatalf("backoff after success = %v, applied=%v; want 1m", got.RetryAfter, applied)
	}
}

func TestOAuthModelEntitlementBackoffIsolatedByAccountAndModel(t *testing.T) {
	const message = "The model is not supported when using Codex with a ChatGPT account."
	tracker := &oauthModelEntitlementBackoffTracker{}
	unsupported := sdk.ForwardOutcome{Kind: sdk.OutcomeClientError, Reason: message}
	request := func(accountID int64, model string) *sdk.ForwardRequest {
		return &sdk.ForwardRequest{
			Account: &sdk.Account{
				ID:          accountID,
				Credentials: map[string]string{"access_token": "oauth-token"},
			},
			DispatchPlan: sdk.DispatchPlan{SchedulingModel: model},
		}
	}

	first := request(1, "gpt-5.6-sol")
	if got, _ := tracker.apply(first, unsupported); got.RetryAfter != time.Minute {
		t.Fatalf("first account/model RetryAfter = %v, want 1m", got.RetryAfter)
	}
	if got, _ := tracker.apply(first, unsupported); got.RetryAfter != 3*time.Minute {
		t.Fatalf("second account/model RetryAfter = %v, want 3m", got.RetryAfter)
	}
	if got, _ := tracker.apply(request(2, "gpt-5.6-sol"), unsupported); got.RetryAfter != time.Minute {
		t.Fatalf("different account RetryAfter = %v, want 1m", got.RetryAfter)
	}
	if got, _ := tracker.apply(request(1, "gpt-5.6-terra"), unsupported); got.RetryAfter != time.Minute {
		t.Fatalf("different model RetryAfter = %v, want 1m", got.RetryAfter)
	}
}
