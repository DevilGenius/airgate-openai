package gateway

import (
	"context"
	"testing"
)

func TestRefreshPlanJWTAuthority(t *testing.T) {
	jwt := func(account, plan string) string {
		return testJWT(t, map[string]any{"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": account, "chatgpt_plan_type": plan,
			"chatgpt_subscription_active_until": "2030-01-01T00:00:00Z",
		}})
	}
	for _, tc := range []struct {
		name, accessAccount, accessPlan, idAccount, idPlan, want string
	}{
		{"access free beats paid ID token", "a", "free", "a", "self_serve_business_prolite", "free"},
		{"access paid beats free ID token", "a", "plus", "a", "free", "plus"},
		{"same account ID token fallback", "a", "", "a", "pro", "pro"},
		{"different workspace cannot supply plan", "a", "", "b", "pro", "unknown"},
		{"missing identity cannot supply plan", "", "", "b", "pro", "unknown"},
		{"missing plan", "a", "", "a", "", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRefreshTokenInfo(jwt(tc.idAccount, tc.idPlan), jwt(tc.accessAccount, tc.accessPlan))
			if got.PlanType != tc.want || got.AccountID != tc.accessAccount {
				t.Fatalf("got plan=%q account=%q, want plan=%q account=%q", got.PlanType, got.AccountID, tc.want, tc.accessAccount)
			}
			if (tc.want == "free" || tc.want == "unknown") && got.SubscriptionActiveUntil != "" {
				t.Fatalf("stale subscription expiration retained: %q", got.SubscriptionActiveUntil)
			}
		})
	}
}

func TestRefreshAccessTokenDoesNotReuseStoredPaidPlan(t *testing.T) {
	for _, plan := range []string{"free", ""} {
		access := testJWT(t, map[string]any{"chatgpt_account_id": "account", "chatgpt_plan_type": plan})
		// A canceled context also prevents accidental upstream enrichment.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		got, err := (&OpenAIGateway{}).RefreshToken(ctx, map[string]string{
			"access_token": access, "plan_type": "self_serve_business_prolite",
			"subscription_active_until": "2030-01-01T00:00:00Z",
		})
		if err != nil {
			t.Fatal(err)
		}
		want := plan
		if want == "" {
			want = "unknown"
		}
		if got.Extra["plan_type"] != want {
			t.Fatalf("plan=%q, want %q", got.Extra["plan_type"], want)
		}
		if expiration, present := got.Extra["subscription_active_until"]; !present || expiration != "" {
			t.Fatalf("missing explicit expiration reset: %+v", got.Extra)
		}
	}
}
