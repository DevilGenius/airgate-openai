package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Reduced from the 2026-09-26 trace: ordinary namespaced tool history plus
// web_search must not cause the proxy to activate Responses Lite.
func TestResponsesPolicyRequiresExplicitLiteOptInAcrossRequestPaths(t *testing.T) {
	paths := []struct {
		name string
		run  func([]byte, http.Header) ([]byte, error)
	}{
		{"policy", func(body []byte, headers http.Header) ([]byte, error) {
			return normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{
				strictCodex: true, finalize: true, model: "gpt-5.6-sol", headers: headers,
			}), nil
		}},
		{"oauth_request", func(body []byte, headers http.Header) ([]byte, error) {
			return buildResponseCreateWSRequestWithHeaders(body, "gpt-5.6-sol", openAISessionResolution{}, headers)
		}},
		{"websocket_message", func(body []byte, headers http.Header) ([]byte, error) {
			body, err := sjson.SetBytes(body, "type", "response.create")
			if err != nil {
				return nil, err
			}
			return sanitizeResponsesWebSocketClientMessage(body, responsesNormalizeOptions{strictCodex: true, headers: headers}), nil
		}},
	}
	markers := []struct {
		name     string
		header   string
		metadata string
		lite     bool
	}{
		{name: "absent"},
		{name: "header_false", header: "false"},
		{name: "metadata_false", metadata: `,"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"false"}`},
		{name: "metadata_boolean_false", metadata: `,"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":false}`},
		{name: "header_true", header: "true", lite: true},
		{name: "metadata_true", metadata: `,"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}`, lite: true},
	}
	for _, path := range paths {
		for _, marker := range markers {
			for _, continuation := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/continuation=%t", path.name, marker.name, continuation), func(t *testing.T) {
					previous := ""
					if continuation {
						previous = `,"previous_response_id":"resp_previous"`
					}
					body := []byte(`{"model":"gpt-5.6-sol","parallel_tool_calls":true,"tool_choice":"auto",
						"reasoning":{"effort":"xhigh"},
						"tools":[{"type":"namespace","name":"workspace","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]},{"type":"web_search","external_web_access":true}],
						"input":[{"type":"function_call","call_id":"call_1","name":"read","namespace":"workspace","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]` + marker.metadata + previous + `}`)
					headers := make(http.Header)
					if marker.header != "" {
						headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", marker.header)
					}
					got, err := path.run(body, headers)
					if err != nil {
						t.Fatal(err)
					}
					if lite := gjson.GetBytes(got, codexResponsesLiteMetadataPath).String() == "true"; lite != marker.lite {
						t.Fatalf("Lite mode = %t, want %t: %s", lite, marker.lite, got)
					}
					parallel := gjson.GetBytes(got, "parallel_tool_calls")
					if !parallel.Exists() || parallel.Bool() == marker.lite {
						t.Fatalf("unexpected parallel_tool_calls: %s", got)
					}
					if context := gjson.GetBytes(got, "reasoning.context"); context.Exists() != marker.lite {
						t.Fatalf("unexpected Lite reasoning context: %s", got)
					}
					if gjson.GetBytes(got, "tools.1.type").String() != "web_search" || !gjson.GetBytes(got, "tools.1.external_web_access").Bool() {
						t.Fatalf("web search was changed: %s", got)
					}
					if gjson.GetBytes(got, "input.#").Int() != 2 || gjson.GetBytes(got, "input.1.call_id").String() != "call_1" {
						t.Fatalf("tool history was lost: %s", got)
					}
					if gjson.GetBytes(got, "tools.0.name").String() != "workspace" || gjson.GetBytes(got, "tool_choice").String() != "auto" {
						t.Fatalf("tool declarations or selection changed: %s", got)
					}
					// Finalization/replay must not infer Lite on a subsequent pass.
					var replay map[string]any
					if err := json.Unmarshal(got, &replay); err != nil {
						t.Fatal(err)
					}
					replay = applyContinuationState(replay, openAISessionResolution{})
					if lite := responsesLiteEnabled(replay, responsesNormalizeOptions{strictCodex: true}); lite != marker.lite {
						t.Fatalf("replay changed Lite mode to %t", lite)
					}
				})
			}
		}
	}
}

func TestResponsesPolicyConvertsMessagesBeforeContinuationFinalization(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.5","previous_response_id":"resp_prev","messages":[{"role":"user","content":"continue"}]}`)
	normalized, err := normalizeWSRequestBody(body, "gpt-5.5", nil)
	if err != nil {
		t.Fatal(err)
	}
	if text := gjson.GetBytes(normalized, "input.0.content.0.text").String(); text != "continue" {
		t.Fatalf("shape phase lost converted message: %s", normalized)
	}
	var reqData map[string]any
	if err := json.Unmarshal(normalized, &reqData); err != nil {
		t.Fatal(err)
	}
	reqData = applyContinuationState(reqData, openAISessionResolution{})
	encoded, _ := json.Marshal(reqData)
	if text := gjson.GetBytes(encoded, "input.0.content.0.text").String(); text != "continue" {
		t.Fatalf("finalization lost converted message: %s", encoded)
	}
}

func TestResponsesPolicyPreservesLiteNamespaceAcrossWebSocketBody(t *testing.T) {
	body := []byte(`{
		"type":"response.create",
		"model":"gpt-5.6-sol",
		"parallel_tool_calls":true,
		"input":[{
			"type":"function_call",
			"id":"fc_server",
			"call_id":"call_1",
			"name":"read",
			"namespace":"workspace",
			"arguments":"{}",
			"status":"completed"
		}]
	}`)

	got := normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{
		strictCodex: true,
		finalize:    true,
		model:       "gpt-5.6-sol",
		headers: http.Header{
			"X-Openai-Internal-Codex-Responses-Lite": []string{"true"},
		},
	})

	if namespace := gjson.GetBytes(got, "input.0.namespace").String(); namespace != "workspace" {
		t.Fatalf("namespace = %q, want workspace; body=%s", namespace, got)
	}
	if gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("server-owned id was not removed: %s", got)
	}
	if status := gjson.GetBytes(got, "input.0.status").String(); status != "completed" {
		t.Fatalf("status changed unexpectedly: %q; body=%s", status, got)
	}
	if parallel := gjson.GetBytes(got, "parallel_tool_calls"); !parallel.Exists() || parallel.Bool() {
		t.Fatalf("parallel_tool_calls = %s, want false; body=%s", parallel.Raw, got)
	}
	if context := gjson.GetBytes(got, "reasoning.context").String(); context != "all_turns" {
		t.Fatalf("reasoning.context = %q, want all_turns; body=%s", context, got)
	}
	if marker := gjson.GetBytes(got, codexResponsesLiteMetadataPath).String(); marker != "true" {
		t.Fatalf("Lite WS marker = %q, want true; body=%s", marker, got)
	}
}

func TestResponsesPolicyRemovesNamespaceForNonLiteCodexModel(t *testing.T) {
	body := []byte(`{
		"type":"response.create",
		"model":"gpt-5.5",
		"input":[{
			"type":"function_call",
			"call_id":"call_1",
			"name":"read",
			"namespace":"workspace",
			"arguments":"{}",
			"status":"completed"
		}],
		"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}
	}`)

	got := normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{
		strictCodex: true,
		model:       "gpt-5.5",
	})

	if gjson.GetBytes(got, "input.0.namespace").Exists() {
		t.Fatalf("namespace should be removed for non-Lite model: %s", got)
	}
	if gjson.GetBytes(got, codexResponsesLiteMetadataPath).Exists() {
		t.Fatalf("stale Lite marker should be removed: %s", got)
	}
	if status := gjson.GetBytes(got, "input.0.status").String(); status != "completed" {
		t.Fatalf("status changed unexpectedly: %q; body=%s", status, got)
	}
}

func TestResponsesPolicyFiltersStatelessServerOutputs(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"message","id":"msg_1","role":"user","content":"continue","status":"completed"},
			{"type":"image_generation_call","id":"ig_1","result":"image","status":"completed"},
			{"type":"web_search_call","id":"ws_1","status":"completed"},
			{"type":"tool_search_output","execution":"server","tools":[],"status":"completed"},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{}","status":"completed"},
			{"type":"function_call_output","id":"fco_1","call_id":"call_1","output":"ok","status":"completed"}
		]
	}`)

	got := normalizeResponsesInputWithOptions(body, "/v1/responses", responsesNormalizeOptions{
		strictCodex: true,
		finalize:    true,
		model:       "gpt-5.6-sol",
	})

	if count := gjson.GetBytes(got, "input.#").Int(); count != 3 {
		t.Fatalf("input count = %d, want message + call + output; body=%s", count, got)
	}
	for index, wantType := range []string{"message", "function_call", "function_call_output"} {
		path := "input." + string(rune('0'+index))
		if typ := gjson.GetBytes(got, path+".type").String(); typ != wantType {
			t.Fatalf("%s.type = %q, want %q; body=%s", path, typ, wantType, got)
		}
		if gjson.GetBytes(got, path+".id").Exists() {
			t.Fatalf("%s retained server-owned id: %s", path, got)
		}
		if status := gjson.GetBytes(got, path+".status").String(); status != "completed" {
			t.Fatalf("%s.status = %q, want unchanged completed; body=%s", path, status, got)
		}
	}
}

func TestResponsesPolicyKeepsStandardAPIShapeOutsideCodex(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","namespace":"workspace","arguments":"{}","status":"completed"}]}`)
	got := normalizeResponsesInput(body, "/v1/responses")

	if namespace := gjson.GetBytes(got, "input.0.namespace").String(); namespace != "workspace" {
		t.Fatalf("standard Responses namespace changed: %s", got)
	}
	if id := gjson.GetBytes(got, "input.0.id").String(); id != "fc_1" {
		t.Fatalf("standard Responses id changed: %s", got)
	}
	if status := gjson.GetBytes(got, "input.0.status").String(); status != "completed" {
		t.Fatalf("standard Responses status changed: %s", got)
	}
}
