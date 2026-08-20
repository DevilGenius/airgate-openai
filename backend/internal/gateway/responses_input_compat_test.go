package gateway

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

var (
	responsesCompatBodySink       []byte
	responsesCompatMapChangedSink bool
)

func TestResponsesPolicyLeavesTextOnlyRequestUnchanged(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("hello ", 256) + `"}]}]}`)
	got := normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{finalize: true})
	if model := gjson.GetBytes(got, "model").String(); model != "gpt-5.4" {
		t.Fatalf("model = %q, want gpt-5.4; body=%s", model, got)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != strings.Repeat("hello ", 256) {
		t.Fatalf("text-only request changed: %s", got)
	}

	reqData := map[string]any{
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "hello"},
				},
			},
		},
	}
	if normalizeResponsesRequestMap(reqData, responsesNormalizeOptions{}) {
		t.Fatalf("decoded text-only request should not change: %#v", reqData)
	}
}

func TestNormalizeResponsesToolCompatibilityRepairsToolDefinitions(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"tools":[
			{"name":"lookup","parameters":{"type":"object"}},
			{"name":"ambiguous","description":"cannot infer custom versus function"},
			{"type":"function","function":{"name":"nested","parameters":{"type":"object"}}},
			{"type":"namespace","name":"crm","tools":[
				{"name":"get_customer","parameters":{"type":"object"}},
				{"type":"function","name":""}
			]}
		],
		"input":[{
			"type":"additional_tools",
			"role":"developer",
			"tools":[
				{"name":"load_order","parameters":{"type":"object"}},
				{"type":"function","name":""}
			]
		}]
	}`)

	got := normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{finalize: true})
	if count := gjson.GetBytes(got, "tools.#").Int(); count != 3 {
		t.Fatalf("top-level tools count = %d, want 3; body=%s", count, got)
	}
	if typ := gjson.GetBytes(got, "tools.0.type").String(); typ != "function" {
		t.Fatalf("inferred tool type = %q, want function; body=%s", typ, got)
	}
	if name := gjson.GetBytes(got, "tools.1.name").String(); name != "nested" {
		t.Fatalf("flattened tool name = %q, want nested; body=%s", name, got)
	}
	if gjson.GetBytes(got, "tools.1.function").Exists() {
		t.Fatalf("nested Chat function wrapper was not removed: %s", got)
	}
	if count := gjson.GetBytes(got, "tools.2.tools.#").Int(); count != 1 {
		t.Fatalf("namespace child count = %d, want 1; body=%s", count, got)
	}
	if typ := gjson.GetBytes(got, "tools.2.tools.0.type").String(); typ != "function" {
		t.Fatalf("namespace child type = %q, want function; body=%s", typ, got)
	}
	if count := gjson.GetBytes(got, "input.0.tools.#").Int(); count != 1 {
		t.Fatalf("additional tools count = %d, want 1; body=%s", count, got)
	}
	if typ := gjson.GetBytes(got, "input.0.tools.0.type").String(); typ != "function" {
		t.Fatalf("additional tool type = %q, want function; body=%s", typ, got)
	}
}

func TestNormalizeResponsesToolCompatibilityDropsInvalidReplayPairs(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]},
			{"type":"function_call","call_id":"call_empty_name","name":"","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_empty_name","output":"bad"},
			{"type":"function_call","call_id":"call_bad_args","name":"lookup","arguments":{}},
			{"type":"function_call_output","call_id":"call_bad_args","output":"bad"},
			{"type":"custom_tool_call","call_id":"call_custom","name":"","input":"patch"},
			{"type":"custom_tool_call_output","call_id":"call_custom","output":"bad"},
			{"type":"function_call","call_id":"call_ok","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_ok","output":"ok"}
		]
	}`)

	got := normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{finalize: true})
	if count := gjson.GetBytes(got, "input.#").Int(); count != 3 {
		t.Fatalf("input count = %d, want message plus valid call/output; body=%s", count, got)
	}
	if callID := gjson.GetBytes(got, "input.1.call_id").String(); callID != "call_ok" {
		t.Fatalf("remaining call_id = %q, want call_ok; body=%s", callID, got)
	}
	if callID := gjson.GetBytes(got, "input.2.call_id").String(); callID != "call_ok" {
		t.Fatalf("remaining output call_id = %q, want call_ok; body=%s", callID, got)
	}
}

func TestNormalizeResponsesToolCompatibilityPreservesAnchoredOutput(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"previous_response_id":"resp_prev",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"},
			{"type":"function_call_output","call_id":"","output":"invalid"}
		]
	}`)

	got := normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{finalize: true})
	if count := gjson.GetBytes(got, "input.#").Int(); count != 1 {
		t.Fatalf("input count = %d, want anchored output only; body=%s", count, got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "function_call_output" {
		t.Fatalf("remaining item type = %q, want function_call_output; body=%s", typ, got)
	}
	if callID := gjson.GetBytes(got, "input.0.call_id").String(); callID != "call_1" {
		t.Fatalf("remaining call_id = %q, want call_1; body=%s", callID, got)
	}
}

func TestBuildCodexWSRequestBackfillsAnchorBeforeDroppingInvalidCall(t *testing.T) {
	body := []byte(`{
		"type":"response.create",
		"model":"gpt-5.4",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`)

	got, err := buildCodexWSRequest(body, "gpt-5.4", openAISessionResolution{PreviousRespID: "resp_prev"})
	if err != nil {
		t.Fatalf("buildCodexWSRequest: %v", err)
	}
	if previous := gjson.GetBytes(got, "previous_response_id").String(); previous != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev; body=%s", previous, got)
	}
	if count := gjson.GetBytes(got, "input.#").Int(); count != 1 {
		t.Fatalf("input count = %d, want anchored output only; body=%s", count, got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "function_call_output" {
		t.Fatalf("remaining item type = %q, want function_call_output; body=%s", typ, got)
	}
}

func TestNormalizeResponsesToolCompatibilityRepairsServerToolSearchOutput(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[{
			"type":"tool_search_output",
			"execution":"server",
			"call_id":null,
			"status":"completed",
			"tools":[{"name":"get_shipping_eta","parameters":{"type":"object"}}]
		}]
	}`)

	got := normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{finalize: true})
	if count := gjson.GetBytes(got, "input.#").Int(); count != 1 {
		t.Fatalf("server tool_search_output was removed: %s", got)
	}
	if typ := gjson.GetBytes(got, "input.0.tools.0.type").String(); typ != "function" {
		t.Fatalf("loaded tool type = %q, want function; body=%s", typ, got)
	}
}

func TestNormalizeResponsesToolCompatibilityDropsChatToolCallWithEmptyName(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		]
	}`)

	got := normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{finalize: true})
	if count := gjson.GetBytes(got, "input.#").Int(); count != 0 {
		t.Fatalf("invalid Chat tool replay was not removed: %s", got)
	}
}

func TestFinalizeAnthropicResponsesBodyDropsToolUseWithEmptyName(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":64,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"","input":{"path":"a.go"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}
		]
	}`)

	converted := convertAnthropicRequestToResponses(body, "gpt-5.4", "")
	got := finalizeAnthropicResponsesBody(converted, body, "")
	for _, item := range gjson.GetBytes(got, "input").Array() {
		if typ := item.Get("type").String(); typ == "function_call" || typ == "function_call_output" {
			t.Fatalf("invalid Anthropic tool replay was not removed: %s", got)
		}
	}
}

func BenchmarkNormalizeResponsesToolCompatibilityTextOnly(b *testing.B) {
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("hello world ", 4096) + `"}]}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		responsesCompatBodySink = normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{finalize: true})
	}
}

func BenchmarkNormalizeResponsesToolCompatibilityMalformedTools(b *testing.B) {
	body := []byte(`{"model":"gpt-5.4","tools":[{"name":"lookup","parameters":{"type":"object"}}],"input":[{"type":"function_call","call_id":"call_1","name":"","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		responsesCompatBodySink = normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{finalize: true})
	}
}

func BenchmarkNormalizeResponsesToolCompatibilityFromMapTextOnly(b *testing.B) {
	reqData := map[string]any{
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "hello"},
				},
			},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		responsesCompatMapChangedSink = normalizeResponsesToolCompatibilityFromMap(reqData)
	}
}
