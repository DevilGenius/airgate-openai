package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestPrepareChatCompletionsStreamUsageAcrossAirGateHops(t *testing.T) {
	original := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	firstHop := prepareChatCompletionsStreamUsage("/v1/chat/completions", true, original)
	if !firstHop.hideUsageFromClient {
		t.Fatal("first AirGate hop should hide the usage chunk it requested internally")
	}
	if !gjson.GetBytes(firstHop.upstreamBody, "stream_options.include_usage").Bool() {
		t.Fatalf("first upstream body did not request usage: %s", firstHop.upstreamBody)
	}

	secondHop := prepareChatCompletionsStreamUsage("/v1/chat/completions", true, firstHop.upstreamBody)
	if secondHop.hideUsageFromClient {
		t.Fatal("upstream AirGate should preserve usage requested by its downstream AirGate client")
	}
	if !gjson.GetBytes(secondHop.upstreamBody, "stream_options.include_usage").Bool() {
		t.Fatalf("second upstream body lost usage request: %s", secondHop.upstreamBody)
	}
	if string(original) == string(firstHop.upstreamBody) {
		t.Fatal("first hop body was not patched")
	}
}

func TestPrepareChatCompletionsStreamUsageScope(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		stream       bool
		body         string
		wantUsage    bool
		wantSuppress bool
	}{
		{name: "explicit false", path: "/v1/chat/completions", stream: true, body: `{"stream_options":{"include_usage":false}}`, wantUsage: true, wantSuppress: true},
		{name: "explicit true", path: "/v1/chat/completions", stream: true, body: `{"stream_options":{"include_usage":true}}`, wantUsage: true, wantSuppress: false},
		{name: "responses", path: "/v1/responses", stream: true, body: `{"stream":true}`, wantUsage: false, wantSuppress: false},
		{name: "non stream", path: "/v1/chat/completions", stream: false, body: `{"stream":false}`, wantUsage: false, wantSuppress: false},
		{name: "invalid json", path: "/v1/chat/completions", stream: true, body: `{`, wantUsage: false, wantSuppress: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := prepareChatCompletionsStreamUsage(tt.path, tt.stream, []byte(tt.body))
			if got := gjson.GetBytes(policy.upstreamBody, "stream_options.include_usage").Bool(); got != tt.wantUsage {
				t.Fatalf("include_usage = %v, want %v; body=%s", got, tt.wantUsage, policy.upstreamBody)
			}
			if policy.hideUsageFromClient != tt.wantSuppress {
				t.Fatalf("hide usage = %v, want %v", policy.hideUsageFromClient, tt.wantSuppress)
			}
		})
	}
}

func TestFilterChatCompletionsUsageForClientKeepsOutputChunk(t *testing.T) {
	data := []byte(`{"id":"chatcmpl_test","choices":[{"delta":{"content":"OK"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	filtered, suppress, changed := filterChatCompletionsUsageForClient(data)
	if suppress || !changed {
		t.Fatalf("filter result suppress=%v changed=%v", suppress, changed)
	}
	if gjson.GetBytes(filtered, "usage").Exists() {
		t.Fatalf("usage was not removed: %s", filtered)
	}
	if got := gjson.GetBytes(filtered, "choices.0.delta.content").String(); got != "OK" {
		t.Fatalf("content = %q, want OK; body=%s", got, filtered)
	}
}

func TestForwardAPIKeyChatStreamMetersInjectedUsageWithoutExposingChunk(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatCompletionsUsageTestStream())
	}))
	defer server.Close()

	recorder := httptest.NewRecorder()
	gateway := &OpenAIGateway{transportPool: NewTransportPool()}
	req := chatCompletionsUsageForwardRequest(server.URL, recorder, false)

	outcome, err := gateway.forwardAPIKey(context.Background(), req, "")
	if err != nil {
		t.Fatalf("forwardAPIKey returned error: %v", err)
	}
	if !gjson.GetBytes(upstreamBody, "stream_options.include_usage").Bool() {
		t.Fatalf("upstream request missing include_usage: %s", upstreamBody)
	}
	assertChatCompletionsUsage(t, outcome.Usage)

	downstream := recorder.Body.String()
	if !strings.Contains(downstream, `"content":"OK"`) || !strings.Contains(downstream, "data: [DONE]") {
		t.Fatalf("downstream stream lost content: %q", downstream)
	}
	if strings.Contains(downstream, `"prompt_tokens":10`) || strings.Contains(downstream, `"choices":[]`) {
		t.Fatalf("internally requested usage chunk leaked downstream: %q", downstream)
	}
}

func TestForwardAPIKeyChatStreamPreservesClientRequestedUsageChunk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatCompletionsUsageTestStream())
	}))
	defer server.Close()

	recorder := httptest.NewRecorder()
	gateway := &OpenAIGateway{transportPool: NewTransportPool()}
	req := chatCompletionsUsageForwardRequest(server.URL, recorder, true)

	outcome, err := gateway.forwardAPIKey(context.Background(), req, "")
	if err != nil {
		t.Fatalf("forwardAPIKey returned error: %v", err)
	}
	assertChatCompletionsUsage(t, outcome.Usage)
	if downstream := recorder.Body.String(); !strings.Contains(downstream, `"prompt_tokens":10`) || !strings.Contains(downstream, `"choices":[]`) {
		t.Fatalf("client-requested usage chunk was removed: %q", downstream)
	}
}

func chatCompletionsUsageForwardRequest(baseURL string, writer http.ResponseWriter, includeUsage bool) *sdk.ForwardRequest {
	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true}`
	if includeUsage {
		body = `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`
	}
	return &sdk.ForwardRequest{
		Account: &sdk.Account{
			ID: 1,
			Credentials: map[string]string{
				"api_key":  "sk-test",
				"base_url": baseURL,
			},
		},
		Body:    []byte(body),
		Headers: http.Header{"X-Forwarded-Path": []string{"/v1/chat/completions"}},
		Model:   "gpt-5.5",
		Stream:  true,
		Writer:  writer,
	}
}

func chatCompletionsUsageTestStream() string {
	return strings.Join([]string{
		`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","model":"gpt-5.5","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}],"usage":null}`,
		"",
		`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","model":"gpt-5.5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`,
		"",
		`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","model":"gpt-5.5","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":17,"total_tokens":27,"prompt_tokens_details":{"cached_tokens":2}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
}

func assertChatCompletionsUsage(t *testing.T, usage *sdk.Usage) {
	t.Helper()
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.Model != "gpt-5.5" || usage.InputTokens != 8 || usage.CachedInputTokens != 2 || usage.OutputTokens != 17 {
		t.Fatalf("usage = %#v", usage)
	}
	if usage.InputPrice <= 0 || usage.CachedInputPrice <= 0 || usage.OutputPrice <= 0 || usage.AccountCost <= 0 {
		t.Fatalf("usage pricing was not filled: %#v", usage)
	}
}
