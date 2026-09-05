package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestForwardOAuthUnknownModelRequestsReroute(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gateway := &OpenAIGateway{logger: logger}
	req := oauthModelRerouteTestRequest("codex-auto-review", "codex-auto-review", "codex-auto-review")

	outcome, err := gateway.Forward(sdk.WithLogger(context.Background(), logger), req)
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	if target, ok := outcome.ModelRerouteClientTarget(); !ok || target != "gpt-5.5" {
		t.Fatalf("Forward() outcome = %+v, want gpt-5.5 model reroute", outcome)
	}
	if outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Fatalf("Forward() status = %d, want %d", outcome.Upstream.StatusCode, http.StatusBadRequest)
	}
	if code := gjson.GetBytes(outcome.Upstream.Body, "error.code").String(); code != oauthModelRerouteCode {
		t.Fatalf("Forward() error code = %q, want %q", code, oauthModelRerouteCode)
	}
}

func TestOAuthModelRerouteOutcome(t *testing.T) {
	tests := []struct {
		name       string
		req        *sdk.ForwardRequest
		path       string
		wantTarget string
	}{
		{
			name: "known model is unchanged",
			req:  oauthModelRerouteTestRequest("gpt-5.5", "gpt-5.5", "gpt-5.5"),
			path: "/v1/responses",
		},
		{
			name: "explicit Luna model is unchanged",
			req:  oauthModelRerouteTestRequest("gpt-5.6-luna", "gpt-5.6-luna", "gpt-5.6-luna"),
			path: "/v1/responses",
		},
		{
			name:       "removed GPT-5.4 reroutes to GPT-5.5",
			req:        oauthModelRerouteTestRequest("gpt-5.4", "gpt-5.4", "gpt-5.4"),
			path:       "/v1/responses",
			wantTarget: "gpt-5.5",
		},
		{
			name: "api key forwarding preserves unknown model",
			req: func() *sdk.ForwardRequest {
				req := oauthModelRerouteTestRequest("codex-auto-review", "codex-auto-review", "codex-auto-review")
				req.Account.Credentials = map[string]string{"api_key": "sk-test"}
				return req
			}(),
			path: "/v1/responses",
		},
		{
			name: "rerouted dispatch plan does not loop on original client model",
			req:  oauthModelRerouteTestRequest("codex-auto-review", "gpt-5.5", "gpt-5.5"),
			path: "/v1/responses",
		},
		{
			name: "wire model controls resolution independently of scheduling pool",
			req:  oauthModelRerouteTestRequest("codex-auto-review", "review-pool", "gpt-5.5"),
			path: "/v1/responses",
		},
		{
			name:       "chat completions uses the same Codex upstream resolution",
			req:        oauthModelRerouteTestRequest("codex-auto-review", "codex-auto-review", "codex-auto-review"),
			path:       "/v1/chat/completions",
			wantTarget: "gpt-5.5",
		},
		{
			name: "responses compact keeps its dedicated mapping",
			req:  oauthModelRerouteTestRequest("codex-auto-review", "codex-auto-review", "codex-auto-review"),
			path: "/v1/responses/compact",
		},
		{
			name: "anthropic mapping remains owned by anthropic dispatch",
			req: func() *sdk.ForwardRequest {
				req := oauthModelRerouteTestRequest("claude-unknown", "claude-unknown", "claude-unknown")
				req.Headers.Set("X-Forwarded-Path", "/v1/messages")
				return req
			}(),
			path: "/v1/messages",
		},
		{
			name: "images keep their dedicated OAuth bridge",
			req:  oauthModelRerouteTestRequest("unknown-image-model", "unknown-image-model", "unknown-image-model"),
			path: "/v1/images/generations",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.req.Headers.Set("X-Forwarded-Path", test.path)
			outcome, ok := oauthModelRerouteOutcome(test.req, test.path)
			if test.wantTarget == "" {
				if ok {
					t.Fatalf("oauthModelRerouteOutcome() = %+v, true; want no reroute", outcome)
				}
				return
			}
			target, valid := outcome.ModelRerouteClientTarget()
			if !ok || !valid || target != test.wantTarget {
				t.Fatalf("oauthModelRerouteOutcome() = %+v, %v; want target %q", outcome, ok, test.wantTarget)
			}
		})
	}
}

func oauthModelRerouteTestRequest(clientModel, schedulingModel, wireModel string) *sdk.ForwardRequest {
	return &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{"access_token": "token"}},
		Body:    []byte(`{"model":"` + clientModel + `","input":"hello"}`),
		Headers: http.Header{
			"Content-Type":     []string{"application/json"},
			"X-Forwarded-Path": []string{"/v1/responses"},
		},
		Model: clientModel,
		DispatchPlan: sdk.DispatchPlan{
			ClientModel:     clientModel,
			SchedulingModel: schedulingModel,
			WireModel:       wireModel,
		},
	}
}
