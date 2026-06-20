package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOAuthCallbackURL(t *testing.T) {
	if got := OAuthCallbackURL(); got != "http://localhost:1455/auth/callback" {
		t.Fatalf("OAuthCallbackURL = %q", got)
	}
}

func TestStartOAuthBuildsAuthorizeURLAndStoresSession(t *testing.T) {
	clearOAuthSessionsForTest(t)
	g := &OpenAIGateway{logger: slog.Default()}

	resp, err := g.StartOAuth(context.Background(), &OAuthStartRequest{})
	if err != nil {
		t.Fatalf("StartOAuth returned err: %v", err)
	}
	if resp.State == "" {
		t.Fatal("state should be generated")
	}
	if _, ok := oauthSessions.Load(resp.State); !ok {
		t.Fatal("PKCE session should be stored")
	}
	parsed, err := url.Parse(resp.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize URL parse failed: %v", err)
	}
	query := parsed.Query()
	if parsed.Scheme != "https" || parsed.Host != "auth.openai.com" {
		t.Fatalf("unexpected authorize endpoint: %s", resp.AuthorizeURL)
	}
	for key, want := range map[string]string{
		"client_id":                 oauthClientID,
		"scope":                     oauthScope,
		"response_type":             "code",
		"redirect_uri":              OAuthCallbackURL(),
		"state":                     resp.State,
		"code_challenge_method":     "S256",
		"codex_cli_simplified_flow": "true",
	} {
		if got := query.Get(key); got != want {
			t.Fatalf("query %s = %q, want %q", key, got, want)
		}
	}
	if query.Get("code_challenge") == "" {
		t.Fatal("code_challenge should be set")
	}
}

func TestHandleOAuthCallbackStateErrors(t *testing.T) {
	clearOAuthSessionsForTest(t)
	g := &OpenAIGateway{logger: slog.Default()}
	if _, err := g.HandleOAuthCallback(context.Background(), &OAuthCallbackRequest{State: "missing"}); err == nil {
		t.Fatal("missing state should fail")
	}

	oauthSessions.Store("expired", &pkceSession{
		verifier:    "verifier",
		callbackURL: OAuthCallbackURL(),
		createdAt:   time.Now().Add(-11 * time.Minute),
	})
	if _, err := g.HandleOAuthCallback(context.Background(), &OAuthCallbackRequest{State: "expired", Code: "code"}); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("expired state error = %v", err)
	}
	if _, ok := oauthSessions.Load("expired"); ok {
		t.Fatal("expired state should be deleted")
	}
}

func TestParseSessionJSONAndCredentials(t *testing.T) {
	for _, raw := range []string{"", "token", `{bad`, `{"user":{}}`} {
		if _, err := parseSessionJSON(raw); err == nil {
			t.Fatalf("parseSessionJSON(%q) should fail", raw)
		}
	}

	raw := `{
		"user":{"id":"user_1","name":"","email":"user@example.com"},
		"expires":"2026-07-01T00:00:00Z",
		"account":{"id":"acct_1","planType":"plus"},
		"accessToken":"at_1",
		"sessionToken":"st_1",
		"authProvider":"openai"
	}`
	sess, err := parseSessionJSON(raw)
	if err != nil {
		t.Fatalf("parseSessionJSON valid returned err: %v", err)
	}
	creds, name := credentialsFromSession(sess)
	if name != "user@example.com" {
		t.Fatalf("account name fallback = %q", name)
	}
	for key, want := range map[string]string{
		"access_token":              "at_1",
		"session_token":             "st_1",
		"chatgpt_account_id":        "acct_1",
		"email":                     "user@example.com",
		"plan_type":                 "plus",
		"subscription_active_until": "2026-07-01T00:00:00Z",
	} {
		if got := creds[key]; got != want {
			t.Fatalf("creds[%s] = %q, want %q", key, got, want)
		}
	}
}

func TestImportFromSessionJSONUsesEmbeddedAccessToken(t *testing.T) {
	g := &OpenAIGateway{logger: slog.Default()}
	raw := `{"user":{"name":"Alice","email":"alice@example.com"},"account":{"id":"acct_2","planType":"team"},"accessToken":"at_2","sessionToken":"st_2"}`

	result, err := g.ImportFromSessionJSON(context.Background(), raw, "")
	if err != nil {
		t.Fatalf("ImportFromSessionJSON returned err: %v", err)
	}
	if result.AccountType != "oauth" || result.AccountName != "Alice" {
		t.Fatalf("unexpected result identity: %#v", result)
	}
	if result.Credentials["access_token"] != "at_2" || result.Credentials["session_token"] != "st_2" {
		t.Fatalf("unexpected credentials: %#v", result.Credentials)
	}

	if _, err := g.ImportFromSessionJSON(context.Background(), "", ""); err == nil {
		t.Fatal("blank session input should fail")
	}
	if _, err := g.ImportFromSessionJSON(context.Background(), `{"accessToken":""}`, ""); err == nil {
		t.Fatal("invalid JSON session should not fall back to session token")
	}
}

func TestTokenResponseErrorMessage(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{raw: `"invalid_grant"`, want: "invalid_grant"},
		{raw: `{"message":"bad code"}`, want: "bad code"},
		{raw: `{"error_description":"expired"}`, want: "expired"},
		{raw: `{"code":"invalid_request"}`, want: "invalid_request"},
		{raw: `{"unknown":"value"}`, want: `{"unknown":"value"}`},
		{raw: `123`, want: `123`},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			tr := &tokenResponse{Error: json.RawMessage(tc.raw)}
			if got := tr.errorMessage(); got != tc.want {
				t.Fatalf("errorMessage = %q, want %q", got, tc.want)
			}
		})
	}
	if got := (&tokenResponse{}).errorMessage(); got != "" {
		t.Fatalf("empty error message = %q", got)
	}
}

func TestParseTokenInfoAndOrganizationFallback(t *testing.T) {
	idToken := testJWT(t, map[string]any{
		"email": "user@example.com",
		"name":  "User",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                "acct_nested",
			"chatgpt_plan_type":                 "plus",
			"chatgpt_subscription_active_until": "2026-07-01T00:00:00Z",
			"organizations": []any{
				map[string]any{"id": "org_first"},
				map[string]any{"id": "org_default", "is_default": true},
			},
		},
	})
	info := parseIDToken(idToken)
	if info.AccountID != "acct_nested" || info.PlanType != "plus" || info.OrganizationID != "org_default" {
		t.Fatalf("parseIDToken nested info = %#v", info)
	}
	if info.AccountName != "User" || info.Email != "user@example.com" {
		t.Fatalf("parseIDToken user info = %#v", info)
	}

	accessToken := testJWT(t, map[string]any{
		"chatgpt_plan_type":                 "team",
		"chatgpt_subscription_active_until": "2026-08-01T00:00:00Z",
		"email":                             "fallback@example.com",
	})
	info = parseTokenInfo(testJWT(t, map[string]any{"chatgpt_account_id": "acct_only"}), accessToken)
	if info.AccountID != "acct_only" || info.PlanType != "team" || info.Email != "fallback@example.com" {
		t.Fatalf("parseTokenInfo fallback = %#v", info)
	}

	for _, token := range []string{"", "bad", "a.bad.c"} {
		if got := parseIDToken(token); got.AccountID != "" || got.PlanType != "" {
			t.Fatalf("invalid token parsed data: %#v", got)
		}
	}
	if got := defaultOrganizationID(map[string]interface{}{"organizations": "bad"}); got != "" {
		t.Fatalf("bad organizations default = %q", got)
	}
	if got := defaultOrganizationID(map[string]interface{}{"organizations": []interface{}{map[string]interface{}{"id": "org_first"}}}); got != "org_first" {
		t.Fatalf("first org default = %q", got)
	}
}

func TestSelectChatGPTAccountInfoFallbacks(t *testing.T) {
	if got := selectChatGPTAccountInfo(map[string]interface{}{}, ""); got != nil {
		t.Fatalf("missing accounts should return nil, got %#v", got)
	}
	result := map[string]interface{}{
		"accounts": map[string]interface{}{
			"org_requested": map[string]interface{}{"account": map[string]interface{}{}},
			"org_free":      map[string]interface{}{"account": map[string]interface{}{"plan_type": "free"}},
			"org_paid":      map[string]interface{}{"entitlement": map[string]interface{}{"subscription_plan": "pro"}},
		},
	}
	info := selectChatGPTAccountInfo(result, "org_requested")
	if info == nil || info.PlanType != "pro" || info.SelectionReason != "first_paid_account" {
		t.Fatalf("paid fallback = %#v", info)
	}
}

func TestNormalizeOAuthClientIDAndImportRefreshValidation(t *testing.T) {
	if got := normalizeOAuthClientID(""); got != oauthClientID {
		t.Fatalf("default client ID = %q", got)
	}
	if got := normalizeOAuthClientID(" custom "); got != "custom" {
		t.Fatalf("custom client ID = %q", got)
	}
	if _, err := (&OpenAIGateway{logger: slog.Default()}).ImportFromRefreshToken(context.Background(), " ", "", ""); err == nil {
		t.Fatal("blank refresh token should fail")
	}
}

func TestCleanExpiredSessions(t *testing.T) {
	clearOAuthSessionsForTest(t)
	oauthSessions.Store("old", &pkceSession{createdAt: time.Now().Add(-11 * time.Minute)})
	oauthSessions.Store("new", &pkceSession{createdAt: time.Now()})
	cleanExpiredSessions()
	if _, ok := oauthSessions.Load("old"); ok {
		t.Fatal("old session should be removed")
	}
	if _, ok := oauthSessions.Load("new"); !ok {
		t.Fatal("new session should remain")
	}
}

func clearOAuthSessionsForTest(t *testing.T) {
	t.Helper()
	oauthSessions.Range(func(key, _ any) bool {
		oauthSessions.Delete(key)
		return true
	})
	t.Cleanup(func() {
		oauthSessions.Range(func(key, _ any) bool {
			oauthSessions.Delete(key)
			return true
		})
	})
}
