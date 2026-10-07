package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestModelPassthroughOAuthRequest(t *testing.T) {
	for _, model := range []string{"codex-auto-review", "vendor/custom-model", "gpt-5.4", "gpt-5.6-sol"} {
		for _, shape := range []string{"responses", "response.create", "chat"} {
			t.Run(model+"/"+shape, func(t *testing.T) {
				payload := map[string]any{"model": model, "input": "Reply OK"}
				if shape == "response.create" {
					payload["type"] = "response.create"
				}
				if shape == "chat" {
					delete(payload, "input")
					payload["messages"] = []any{map[string]any{"role": "user", "content": "Reply OK"}}
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				// Empty req.Model also exercises fallback to the original body model.
				for _, selected := range []string{model, ""} {
					req := &sdk.ForwardRequest{Model: selected, Body: body, Headers: http.Header{}}
					wire, err := (&OpenAIGateway{}).buildWSRequest(req, openAISessionResolution{})
					if err != nil {
						t.Fatal(err)
					}
					if got := gjson.GetBytes(wire, "model").String(); got != model {
						t.Fatalf("upstream model = %q, want %q", got, model)
					}
				}
			})
		}
	}
}

func TestModelPassthroughAPIKeyForward(t *testing.T) {
	for _, model := range []string{"codex-auto-review", "vendor/custom-model", "gpt-5.4", "gpt-5.6-sol"} {
		for _, path := range []string{"/v1/responses", "/responses", "/v1/chat/completions"} {
			t.Run(model+path, func(t *testing.T) {
				received := make(chan string, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					received <- gjson.GetBytes(body, "model").String()
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_test", "model": model, "status": "completed", "output": []any{}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 1}})
				}))
				defer server.Close()
				payload := map[string]any{"model": model, "input": "Reply OK", "stream": false}
				if path == "/v1/chat/completions" {
					delete(payload, "input")
					payload["messages"] = []any{map[string]any{"role": "user", "content": "Reply OK"}}
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				req := &sdk.ForwardRequest{
					Account: &sdk.Account{ID: 1, Credentials: map[string]string{"api_key": "test", "base_url": server.URL}},
					Model:   model, Body: body, Headers: http.Header{"X-Forwarded-Path": {path}},
					DispatchPlan: sdk.DispatchPlan{ClientModel: model, SchedulingModel: model, WireModel: model},
				}
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				gateway := &OpenAIGateway{transportPool: NewTransportPool(), logger: logger}
				defer gateway.transportPool.CloseIdle()
				outcome, err := gateway.Forward(sdk.WithLogger(context.Background(), logger), req)
				if err != nil || outcome.Kind != sdk.OutcomeSuccess {
					t.Fatalf("Forward = %+v, %v", outcome, err)
				}
				select {
				case got := <-received:
					if got != model {
						t.Fatalf("upstream model = %q, want %q", got, model)
					}
				default:
					t.Fatal("no upstream request")
				}
			})
		}
	}
}
