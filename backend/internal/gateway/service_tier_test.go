package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type serviceTierTestProtocol struct {
	name      string
	path      string
	stream    bool
	chat      bool
	anthropic bool
}

var serviceTierTestProtocols = []serviceTierTestProtocol{
	{name: "responses-json", path: "/v1/responses"},
	{name: "responses-sse", path: "/v1/responses", stream: true},
	{name: "compact-json", path: "/v1/responses/compact"},
	{name: "chat-json", path: "/v1/chat/completions", chat: true},
	{name: "chat-sse", path: "/v1/chat/completions", chat: true, stream: true},
	{name: "anthropic-json", path: "/v1/messages", anthropic: true},
	{name: "anthropic-sse", path: "/v1/messages", anthropic: true, stream: true},
}

func TestAPIKeyServiceTierForwardingAndBilling(t *testing.T) {
	cases := []struct {
		name, requested, reported, wire, billed string
		priceMultiplier                         float64
	}{
		{"omitted-default", "", "default", "default", "", 1},
		{"omitted-priority", "", "priority", "default", "", 1},
		{"omitted-fast", "", "fast", "default", "", 1},
		{"omitted-missing", "", "", "default", "", 1},
		{"default-priority", "default", "priority", "default", "", 1},
		{"default-fast", "default", "fast", "default", "", 1},
		{"default-flex", "default", "flex", "default", "", 1},
		{"priority-confirmed", "priority", "priority", "priority", "priority", 2},
		{"priority-downgraded", "priority", "default", "priority", "", 1},
		{"priority-auto-falls-back", "priority", "auto", "priority", "priority", 2},
		{"priority-missing", "priority", "", "priority", "priority", 2},
		{"priority-served-as-flex", "priority", "flex", "priority", "flex", 0.5},
		{"priority-served-as-fast", "priority", "fast", "priority", "priority", 2},
		{"normalized-priority", " PRIORITY ", " PRIORITY ", "priority", "priority", 2},
		{"fast-confirmed", "fast", "priority", "priority", "priority", 2},
		{"fast-downgraded", "fast", "default", "priority", "", 1},
		{"fast-missing", "fast", "", "priority", "priority", 2},
		{"fast-response-alias", "fast", "fast", "priority", "priority", 2},
		{"fast-served-as-flex", "fast", "flex", "priority", "flex", 0.5},
		{"normalized-fast", " FaSt ", " FAST ", "priority", "priority", 2},
		{"flex-confirmed", "flex", "flex", "flex", "flex", 0.5},
		{"flex-served-as-default", "flex", "default", "flex", "", 1},
		{"flex-served-as-priority", "flex", "priority", "flex", "priority", 2},
		{"flex-served-as-fast", "flex", "fast", "flex", "priority", 2},
		{"flex-missing", "flex", "", "flex", "flex", 0.5},
	}
	for _, protocol := range serviceTierTestProtocols {
		for _, tc := range cases {
			t.Run(protocol.name+"/"+tc.name, func(t *testing.T) {
				outcome, wireBody := forwardServiceTierTest(t, protocol, tc.requested, tc.reported, "response.completed", "")
				if outcome.Kind != sdk.OutcomeSuccess {
					t.Fatalf("outcome = %s: %s", outcome.Kind, outcome.Reason)
				}
				if got := gjson.GetBytes(wireBody, "service_tier").String(); got != tc.wire {
					t.Fatalf("upstream service_tier = %q, want %q; body=%s", got, tc.wire, wireBody)
				}
				assertServiceTierTestUsage(t, outcome.Usage, tc.billed, tc.priceMultiplier)
			})
		}
	}
}

func TestAPIKeyServiceTierTerminalEvents(t *testing.T) {
	for _, terminal := range []string{"response.done", "response.incomplete", "response.failed"} {
		for _, protocol := range []serviceTierTestProtocol{serviceTierTestProtocols[1], serviceTierTestProtocols[6]} {
			for _, tc := range []struct {
				requested, reported, billed string
				multiplier                  float64
			}{
				{"default", "priority", "", 1},
				{"priority", "default", "", 1},
				{"priority", "priority", "priority", 2},
				{"fast", "default", "", 1},
				{"fast", "priority", "priority", 2},
				{"fast", "flex", "flex", 0.5},
				{"flex", "default", "", 1},
				{"flex", "priority", "priority", 2},
			} {
				t.Run(protocol.name+"/"+terminal+"/"+tc.requested+"/"+tc.reported, func(t *testing.T) {
					outcome, _ := forwardServiceTierTest(t, protocol, tc.requested, tc.reported, terminal, "")
					assertServiceTierTestUsage(t, outcome.Usage, tc.billed, tc.multiplier)
				})
			}
		}
	}
}

func TestAPIKeyChatServiceTierReportedBeforeUsage(t *testing.T) {
	for _, tc := range []struct {
		name, requested, final, billed string
		multiplier                     float64
	}{
		{"confirmed-in-earlier-chunk", "priority", "", "priority", 2},
		{"later-downgrade", "priority", "default", "", 1},
		{"fast-later-downgrade", "fast", "default", "", 1},
		{"fast-later-flex", "fast", "flex", "flex", 0.5},
		{"flex-later-default", "flex", "default", "", 1},
		{"flex-priority-confirmed-early", "flex", "", "priority", 2},
		{"no-unrequested-upgrade", "", "", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, _ := forwardServiceTierTest(t, serviceTierTestProtocols[4], tc.requested, tc.final, "response.completed", "priority")
			assertServiceTierTestUsage(t, outcome.Usage, tc.billed, tc.multiplier)
		})
	}
}

func TestOAuthServiceTierUsesSharedBillingPolicy(t *testing.T) {
	for _, protocol := range serviceTierTestProtocols[:2] {
		for _, tc := range []struct {
			name, requested, reported, billed string
			multiplier                        float64
		}{
			{"priority-downgraded", "priority", "default", "", 1},
			{"no-unrequested-priority", "", "priority", "", 1},
			{"priority-unreported", "priority", "", "priority", 2},
			{"priority-confirmed", "priority", "priority", "priority", 2},
			{"fast-confirmed", "fast", "priority", "priority", 2},
			{"fast-unreported", "fast", "", "priority", 2},
			{"fast-downgraded", "fast", "default", "", 1},
			{"fast-served-as-flex", "fast", "flex", "flex", 0.5},
			{"flex-served-as-default", "flex", "default", "", 1},
			{"flex-served-as-priority", "flex", "priority", "priority", 2},
			{"flex-unreported", "flex", "", "flex", 0.5},
		} {
			t.Run(protocol.name+"/"+tc.name, func(t *testing.T) {
				req := &sdk.ForwardRequest{
					Account: &sdk.Account{Credentials: map[string]string{"access_token": "test-oauth"}},
					Body:    []byte(`{"model":"gpt-5.6-sol"}`),
					Headers: http.Header{"X-Forwarded-Path": {protocol.path}},
				}
				if tc.requested != "" {
					req.Body = []byte(`{"model":"gpt-5.6-sol","input":"hello","service_tier":"` + tc.requested + `"}`)
				}
				wireBody := preprocessRequestBody(req.Body, "gpt-5.6-sol", protocol.path)
				wantWire := tc.requested
				if wantWire == "" {
					wantWire = "default"
				} else if wantWire == "fast" {
					wantWire = "priority"
				}
				if got := gjson.GetBytes(wireBody, "service_tier").String(); got != wantWire {
					t.Fatalf("OAuth wire service_tier = %q, want %q", got, wantWire)
				}
				response := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{},
					Request:    httptest.NewRequest(http.MethodPost, protocol.path, nil),
					Body:       io.NopCloser(strings.NewReader(serviceTierTestResponse(t, protocol, tc.reported, "response.completed", ""))),
				}
				var outcome sdk.ForwardOutcome
				var err error
				if protocol.stream {
					outcome, err = handleStreamResponse(response, httptest.NewRecorder(), time.Now(), tc.requested)
				} else {
					outcome, err = handleNonStreamResponse(response, nil, time.Now(), tc.requested)
				}
				if err != nil || outcome.Kind != sdk.OutcomeSuccess {
					t.Fatalf("OAuth response: %s, %v", outcome.Kind, err)
				}
				assertServiceTierTestUsage(t, outcome.Usage, tc.billed, tc.multiplier)
			})
		}
	}
}

func TestServiceTierDoesNotModifyNonTextRequests(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits", "/v1/audio/transcriptions", "/v1/models"} {
		t.Run(path, func(t *testing.T) {
			req := &sdk.ForwardRequest{
				Account: &sdk.Account{Credentials: map[string]string{"api_key": "test"}},
				Body:    []byte(`{"model":"test","prompt":"hello"}`),
				Headers: http.Header{"X-Forwarded-Path": {path}},
			}
			if got := preprocessRequestBody(req.Body, "", path); string(got) != string(req.Body) {
				t.Fatalf("non-text request gained service_tier: %s", got)
			}
		})
	}
}

func TestOAuthWebSocketServiceTier(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.done", "response.incomplete", "response.failed"} {
		for _, tc := range []struct {
			name, requested, reported, billed string
			multiplier                        float64
		}{
			{"default-not-upgraded", "", "priority", "", 1},
			{"priority-unreported", "priority", "", "priority", 2},
			{"priority-downgraded", "priority", "default", "", 1},
			{"priority-confirmed", "priority", "priority", "priority", 2},
			{"fast-confirmed", "fast", "priority", "priority", 2},
			{"fast-response-alias", "fast", "fast", "priority", 2},
			{"fast-unreported", "fast", "", "priority", 2},
			{"fast-downgraded", "fast", "default", "", 1},
			{"fast-served-as-flex", "fast", "flex", "flex", 0.5},
			{"flex-served-as-default", "flex", "default", "", 1},
			{"flex-served-as-priority", "flex", "priority", "priority", 2},
			{"flex-unreported", "flex", "", "flex", 0.5},
		} {
			t.Run(terminal+"/"+tc.name, func(t *testing.T) {
				payload := map[string]any{"model": "gpt-5.6-sol", "input": "hello"}
				if tc.requested != "" {
					payload["service_tier"] = tc.requested
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				req := &sdk.ForwardRequest{Body: body, Model: "gpt-5.6-sol", Headers: http.Header{}}
				wire, err := (&OpenAIGateway{}).buildWSRequest(req, openAISessionResolution{})
				if err != nil {
					t.Fatal(err)
				}
				wireTier := gjson.GetBytes(wire, "service_tier").String()
				wantWire := tc.requested
				if wantWire == "" {
					wantWire = "default"
				} else if wantWire == "fast" {
					wantWire = "priority"
				}
				if wireTier != wantWire {
					t.Fatalf("WS wire tier = %q, want %q", wireTier, wantWire)
				}
				frames := serviceTierTestResponse(t, serviceTierTestProtocols[1], tc.reported, terminal, "")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					for _, line := range strings.Split(frames, "\n") {
						if data, ok := extractSSEData(line); ok {
							if err := conn.WriteMessage(websocket.TextMessage, []byte(data)); err != nil {
								return
							}
						}
					}
				}))
				defer server.Close()
				conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
				if response != nil && response.Body != nil {
					defer response.Body.Close()
				}
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				result := ReceiveWSResponse(ctx, conn, nil)
				wantReported := tc.reported
				if wantReported == "fast" {
					wantReported = "priority"
				}
				if result.ServiceTier != wantReported {
					t.Fatalf("terminal WS tier = %q, want %q; err=%v", result.ServiceTier, wantReported, result.Err)
				}
				usage := newTokenUsage(result.Model, resolveOpenAIUsageServiceTier(wireTier, result.ServiceTier),
					result.InputTokens, result.OutputTokens, result.CachedInputTokens, result.CacheCreationTokens, result.ReasoningOutputTokens, 0)
				fillUsageCost(usage)
				assertServiceTierTestUsage(t, usage, tc.billed, tc.multiplier)
			})
		}
	}
}

func forwardServiceTierTest(t *testing.T, protocol serviceTierTestProtocol, requested, reported, terminal, earlyChatTier string, groupTier ...string) (sdk.ForwardOutcome, []byte) {
	t.Helper()
	responseBody := serviceTierTestResponse(t, protocol, reported, terminal, earlyChatTier)
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- body
		if protocol.stream || protocol.anthropic {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = io.WriteString(w, responseBody)
	}))
	defer server.Close()
	payload := map[string]any{"model": "gpt-5.6-sol", "stream": protocol.stream}
	if protocol.chat || protocol.anthropic {
		payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	} else {
		payload["input"] = "hello"
	}
	if protocol.anthropic {
		payload["model"] = "claude-sonnet-4-6"
		payload["max_tokens"] = 128
	}
	if requested != "" {
		payload["service_tier"] = requested
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: time.Now().UnixNano(), Type: "apikey", Credentials: map[string]string{"api_key": "test", "base_url": server.URL}},
		Body:    body,
		Model:   "gpt-5.6-sol",
		Headers: http.Header{
			"Content-Type":       {"application/json"},
			"X-Forwarded-Path":   {protocol.path},
			"X-Forwarded-Method": {http.MethodPost},
		},
		Stream:       protocol.stream,
		DispatchPlan: sdk.DispatchPlan{SchedulingModel: "gpt-5.6-sol", WireModel: "gpt-5.6-sol"},
	}
	if len(groupTier) > 0 {
		req.Headers.Set("X-Airgate-Service-Tier", groupTier[0])
	}
	if protocol.stream {
		req.Writer = httptest.NewRecorder()
	}
	gateway := &OpenAIGateway{transportPool: NewTransportPool()}
	defer gateway.transportPool.CloseIdle()
	outcome, err := gateway.forwardHTTP(context.Background(), req)
	if err != nil {
		t.Fatalf("forwardHTTP: %v (kind=%s, reason=%s)", err, outcome.Kind, outcome.Reason)
	}
	select {
	case wireBody := <-received:
		return outcome, wireBody
	default:
		t.Fatalf("no upstream request: kind=%s reason=%s", outcome.Kind, outcome.Reason)
		return sdk.ForwardOutcome{}, nil
	}
}

func serviceTierTestResponse(t *testing.T, protocol serviceTierTestProtocol, reported, terminal, earlyChatTier string) string {
	t.Helper()
	encode := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	usage := map[string]any{
		"input_tokens": 100, "output_tokens": 50,
		"input_tokens_details":        map[string]any{"cached_tokens": 20},
		"cache_creation_input_tokens": 10,
	}
	response := map[string]any{
		"id": "resp_service_tier", "model": "gpt-5.6-sol", "status": "completed", "usage": usage,
		"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "OK"}}}},
	}
	if reported != "" {
		response["service_tier"] = reported
	}
	if protocol.chat {
		response["choices"] = []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}
		response["usage"] = map[string]any{"prompt_tokens": 100, "completion_tokens": 50, "prompt_tokens_details": map[string]any{"cached_tokens": 20}, "cache_creation_input_tokens": 10}
		if !protocol.stream {
			return encode(response)
		}
		first := map[string]any{"model": "gpt-5.6-sol", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "OK"}}}}
		if earlyChatTier != "" {
			first["service_tier"] = earlyChatTier
		}
		delete(response, "output")
		response["choices"] = []any{}
		return "data: " + encode(first) + "\n\ndata: " + encode(response) + "\n\ndata: [DONE]\n\n"
	}
	if !protocol.stream && !protocol.anthropic {
		return encode(response)
	}
	if terminal == "response.failed" {
		response["status"] = "failed"
		response["error"] = map[string]any{"code": "server_error", "message": "test upstream failure"}
	} else if terminal == "response.incomplete" {
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	// An early priority echo must never override the completed response tier.
	return "data: " + encode(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_service_tier", "service_tier": "priority"}}) + "\n\n" +
		"data: " + encode(map[string]any{"type": "response.output_text.delta", "delta": "OK", "output_index": 0, "content_index": 0}) + "\n\n" +
		"data: " + encode(map[string]any{"type": terminal, "response": response}) + "\n\n"
}

func assertServiceTierTestUsage(t *testing.T, usage *sdk.Usage, tier string, multiplier float64) {
	t.Helper()
	if usage == nil {
		t.Fatal("missing usage")
	}
	if got := usage.Metadata[usageAttrServiceTier]; got != tier {
		t.Fatalf("billed service_tier = %q, want %q", got, tier)
	}
	if usage.InputTokens != 70 || usage.CachedInputTokens != 20 || usage.CacheCreationTokens != 10 || usage.OutputTokens != 50 {
		t.Fatalf("unexpected token buckets: %+v", usage)
	}
	assertUsagePricesAndCosts(t, usage, 5*multiplier, 0.5*multiplier, 6.25*multiplier, 30*multiplier)
}
