package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/DevilGenius/airgate-openai/backend/internal/authcompat"
)

func TestCompatibleRefreshTokenInputsSupportsPlainTextAndJSON(t *testing.T) {
	inputs, issues, err := compatibleRefreshTokenInputs([]authcompat.InputFile{
		{Name: "plain.txt", Content: []byte("rt-one\nrt-two")},
		{Name: "options.json", Content: []byte(`{"name":"Custom Account","refresh_token":"rt-three","proxy_url":"http://proxy","client_id":"client"}`)},
		{Name: "empty.txt"},
	})
	if err != nil {
		t.Fatalf("compatibleRefreshTokenInputs() error = %v", err)
	}
	if len(inputs) != 3 || inputs[0].RefreshToken != "rt-one" || inputs[1].RefreshToken != "rt-two" {
		t.Fatalf("plain inputs = %+v", inputs)
	}
	if inputs[2].Name != "Custom Account" || inputs[2].RefreshToken != "rt-three" || inputs[2].ProxyURL != "http://proxy" || inputs[2].ClientID != "client" {
		t.Fatalf("JSON input = %+v", inputs[2])
	}
	if len(issues) != 1 || issues[0].File != "empty.txt" {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestCompatibleImportsResolvePlanAfterRefreshAndFromFullCredentials(t *testing.T) {
	previous := compatibleImportFromRefreshToken
	t.Cleanup(func() { compatibleImportFromRefreshToken = previous })
	calls := 0
	access := testJWT(t, map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "plus"}})
	compatibleImportFromRefreshToken = func(_ *OpenAIGateway, _ context.Context, token, _, _ string) (*OAuthResult, error) {
		calls++
		if token != "rt-only" {
			t.Errorf("unexpected refresh token")
		}
		return &OAuthResult{AccountType: "oauth", Credentials: map[string]string{"access_token": access, "refresh_token": "rotated", "plan_type": "stale"}}, nil
	}
	g := &OpenAIGateway{logger: slog.Default()}
	for _, tc := range []struct {
		format, content string
		wantCalls       int
	}{
		{"refresh_token", "rt-only", 1},
		{"account_json", `{"credentials":{"refresh_token":"rt-only","plan_type":"stale"}}`, 2},
		{"account_json", `{"credentials":{"access_token":"` + access + `","refresh_token":"do-not-refresh","plan_type":"stale"}}`, 2},
	} {
		body, err := json.Marshal(map[string]any{"format": tc.format, "files": []map[string]string{{"name": "account.json", "content": tc.content}}})
		if err != nil {
			t.Fatal(err)
		}
		status, _, response, err := g.handleCompatibleAccountImport(t.Context(), http.MethodPost, body)
		var parsed authcompat.Result
		if err != nil || status != http.StatusOK || json.Unmarshal(response, &parsed) != nil || len(parsed.Accounts) != 1 {
			t.Fatalf("format=%s status=%d err=%v response=%s", tc.format, status, err, response)
		}
		if calls != tc.wantCalls || parsed.Accounts[0].Credentials["plan_type"] != "plus" || parsed.Accounts[0].Name != "" {
			t.Fatalf("format=%s calls=%d: plan/name resolution failed", tc.format, calls)
		}
	}
}

func TestCompatibleRefreshTokenAccountPreservesClientName(t *testing.T) {
	account := compatibleRefreshTokenAccount(compatibleRefreshTokenInput{
		File: "account.json",
		Name: " Client Name ",
	}, &OAuthResult{
		AccountType: "oauth",
		AccountName: "upstream-name",
		Credentials: map[string]string{"access_token": "access"},
	})
	renamed := authcompat.NewImportPreparer().Prepare([]authcompat.Account{account})
	if len(renamed) != 1 || renamed[0].Name != "Client Name" {
		t.Fatalf("renamed account = %+v", renamed)
	}
	if renamed[0].Credentials["account_name"] != "" || renamed[0].Priority != 1 || renamed[0].MaxConcurrency != 15 {
		t.Fatalf("renamed defaults = %+v", renamed[0])
	}
}

func TestCompatiblePlanOverridesUploadedMetadata(t *testing.T) {
	for _, tc := range []struct{ name, plan, want string }{
		{"free overrides old prolite", "free", "free"},
		{"missing JWT plan does not retain old plan", "", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			access := testJWT(t, map[string]any{"chatgpt_account_id": "target", "chatgpt_plan_type": tc.plan})
			id := testJWT(t, map[string]any{"chatgpt_account_id": "other", "chatgpt_plan_type": "pro"})
			file, err := json.Marshal(map[string]any{"credentials": map[string]string{
				"access_token": access, "id_token": id, "plan_type": "self_serve_business_prolite",
				"subscription_active_until": "2030-01-01T00:00:00Z",
			}})
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"format": "account_json", "files": []map[string]string{{"name": "account.json", "content": string(file)}}})
			if err != nil {
				t.Fatal(err)
			}
			status, _, response, err := (&OpenAIGateway{}).handleCompatibleAccountImport(t.Context(), http.MethodPost, body)
			var parsed authcompat.Result
			if err != nil || status != http.StatusOK || json.Unmarshal(response, &parsed) != nil || len(parsed.Accounts) != 1 {
				t.Fatalf("import failed: status=%d err=%v", status, err)
			}
			credentials := parsed.Accounts[0].Credentials
			if credentials["plan_type"] != tc.want || credentials["subscription_active_until"] != "" {
				t.Fatalf("plan=%q expiration=%q", credentials["plan_type"], credentials["subscription_active_until"])
			}
		})
	}
}

func TestHandleCompatibleAccountImportSupportsRTAlias(t *testing.T) {
	previous := compatibleImportFromRefreshToken
	compatibleImportFromRefreshToken = func(_ *OpenAIGateway, _ context.Context, token, proxyURL, clientID string) (*OAuthResult, error) {
		if token == "rt-bad" {
			return nil, errors.New("invalid refresh token")
		}
		if proxyURL != "" || clientID != "" {
			t.Fatalf("unexpected options: proxy=%q client=%q", proxyURL, clientID)
		}
		return &OAuthResult{
			AccountType: "oauth",
			AccountName: "account@example.com",
			Credentials: map[string]string{
				"access_token":  "access",
				"refresh_token": token,
				"email":         "account@example.com",
			},
		}, nil
	}
	defer func() { compatibleImportFromRefreshToken = previous }()

	gateway := &OpenAIGateway{logger: slog.Default()}
	status, _, body, err := gateway.handleCompatibleAccountImport(
		t.Context(),
		http.MethodPost,
		[]byte(`{"format":"rt","files":[{"name":"tokens.txt","content":"rt-good\nrt-bad"}]}`),
	)
	if err != nil || status != http.StatusOK {
		t.Fatalf("handleCompatibleAccountImport() = status %d, err %v, body %s", status, err, body)
	}
	text := string(body)
	if !strings.Contains(text, `"format":"refresh_token"`) ||
		!strings.Contains(text, `"refresh_token":"rt-good"`) ||
		!strings.Contains(text, `"issues"`) {
		t.Fatalf("response body = %s", text)
	}
}
