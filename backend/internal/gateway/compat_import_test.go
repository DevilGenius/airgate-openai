package gateway

import (
	"context"
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
		{Name: "options.json", Content: []byte(`{"refresh_token":"rt-three","proxy_url":"http://proxy","client_id":"client"}`)},
		{Name: "empty.txt"},
	})
	if err != nil {
		t.Fatalf("compatibleRefreshTokenInputs() error = %v", err)
	}
	if len(inputs) != 3 || inputs[0].RefreshToken != "rt-one" || inputs[1].RefreshToken != "rt-two" {
		t.Fatalf("plain inputs = %+v", inputs)
	}
	if inputs[2].RefreshToken != "rt-three" || inputs[2].ProxyURL != "http://proxy" || inputs[2].ClientID != "client" {
		t.Fatalf("JSON input = %+v", inputs[2])
	}
	if len(issues) != 1 || issues[0].File != "empty.txt" {
		t.Fatalf("issues = %+v", issues)
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
