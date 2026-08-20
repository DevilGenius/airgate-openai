package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
)

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
			"X-OpenAI-Internal-Codex-Responses-Lite": []string{"true"},
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
