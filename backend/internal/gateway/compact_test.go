package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestForwardHTTPAPIKeyCompactUsesCompactEndpoint(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","model":"gpt-5.4","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	g := &OpenAIGateway{transportPool: NewTransportPool()}
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{
			ID: 1,
			Credentials: map[string]string{
				"api_key":  "sk-test",
				"base_url": server.URL,
			},
		},
		Body:         []byte(`{"model":"gpt-5.4-openai-compact","stream":false,"input":"hello"}`),
		Headers:      http.Header{"X-Forwarded-Path": []string{"/v1/responses/compact"}},
		Model:        "gpt-5.4",
		DispatchPlan: sdk.DispatchPlan{SchedulingModel: "gpt-5.4", WireModel: "gpt-5.4", RuleID: "responses-compact"},
	}

	outcome, err := g.forwardHTTP(context.Background(), req)
	if err != nil {
		t.Fatalf("forwardHTTP returned error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %s, want success", outcome.Kind)
	}
	if gotPath != "/v1/responses/compact" {
		t.Fatalf("path = %q, want /v1/responses/compact", gotPath)
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("stream should be removed before upstream compact request: %s", gotBody)
	}
	if model := gjson.GetBytes(gotBody, "model").String(); model != "gpt-5.4" {
		t.Fatalf("upstream compact model = %q, want gpt-5.4; body=%s", model, gotBody)
	}
	if input := gjson.GetBytes(gotBody, "input"); input.Type != gjson.String || input.String() != "hello" {
		t.Fatalf("compact input should stay unchanged, got %s in %s", input.Raw, gotBody)
	}
	if object := gjson.GetBytes(outcome.Upstream.Body, "object").String(); object != "response.compaction" {
		t.Fatalf("response object = %q, want response.compaction; body=%s", object, outcome.Upstream.Body)
	}
	if outcome.Usage == nil {
		t.Fatalf("usage is nil")
	}
	if outcome.Usage.Model != "gpt-5.4" {
		t.Fatalf("usage model = %q, want gpt-5.4", outcome.Usage.Model)
	}
	if got := outcome.Usage.Metadata["openai.wire_model"]; got != "" {
		t.Fatalf("wire model metadata = %q, want empty", got)
	}
}

func TestForwardHTTPCompactRejectsStreaming(t *testing.T) {
	g := &OpenAIGateway{}
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{
			ID:          1,
			Credentials: map[string]string{"api_key": "sk-test"},
		},
		Body:         []byte(`{"model":"gpt-5.4","stream":true}`),
		Headers:      http.Header{"X-Forwarded-Path": []string{"/v1/responses/compact"}},
		Model:        "gpt-5.4",
		DispatchPlan: sdk.DispatchPlan{SchedulingModel: "gpt-5.4", WireModel: "gpt-5.4", RuleID: "responses-compact"},
		Stream:       true,
	}

	outcome, err := g.forwardHTTP(context.Background(), req)
	if err != nil {
		t.Fatalf("forwardHTTP returned error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError {
		t.Fatalf("outcome kind = %s, want client_error", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", outcome.Upstream.StatusCode)
	}
	if msg := gjson.GetBytes(outcome.Upstream.Body, "error.message").String(); msg != "streaming not supported for /responses/compact" {
		t.Fatalf("error message = %q", msg)
	}
}
