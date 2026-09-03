package authcompat

import (
	"testing"
	"time"
)

func TestProLiteAccountsReceiveDistinctEmailAliases(t *testing.T) {
	email := "shared@example.com"
	accounts := []Account{
		{
			Email: &email,
			Credentials: map[string]string{
				"plan_type":          "SELF_SERVE_BUSINESS_PROLITE",
				"chatgpt_account_id": "account-1",
			},
		},
		{
			Email: &email,
			Credentials: map[string]string{
				"plan_type":          "ChatGPT ProLite",
				"chatgpt_account_id": "account-2",
			},
		},
	}

	got := NewRenamer().Rename(accounts, time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC))
	if got[0].Email == nil || *got[0].Email != email {
		t.Fatalf("first ProLite email = %v, want %q", got[0].Email, email)
	}
	if got[1].Email == nil || *got[1].Email != "shared+1@example.com" {
		t.Fatalf("second ProLite email = %v, want shared+1@example.com", got[1].Email)
	}
	if got[1].Credentials["email"] != "shared+1@example.com" {
		t.Fatalf("second ProLite credential email = %q", got[1].Credentials["email"])
	}
	if got[0].Name != "0904-ProLite-1" || got[1].Name != "0904-ProLite-2" {
		t.Fatalf("ProLite account names = %q, %q", got[0].Name, got[1].Name)
	}
}

func TestAliasPlanEligibleRecognizesProLiteSpellings(t *testing.T) {
	for _, plan := range []string{"ProLite", "SelfServeBusinessProlite", "ChatgptProlite"} {
		if !aliasPlanEligible(plan) {
			t.Fatalf("aliasPlanEligible(%q) = false", plan)
		}
	}
}
