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

	wantSequence := append(oauthModelEntitlementBackoffSchedule[:], 2*time.Hour)
	for index, want := range wantSequence {
		got, applied := tracker.apply(req, unsupported)
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
	got, applied := tracker.apply(req, unsupported)
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
