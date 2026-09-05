package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// TestIsAnthropicRequest 只认两个权威信号：X-Forwarded-Path + Anthropic-Version 头。
// body 启发式已废除（见 isAnthropicRequest 注释）。
func TestIsAnthropicRequest(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		body    []byte
		want    bool
	}{
		// path 命中 Anthropic
		{
			name:    "path=/v1/messages",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages"}},
			body:    []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    true,
		},
		{
			name:    "path=/v1/messages/count_tokens（子路径）",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages/count_tokens"}},
			body:    []byte(`{"model":"claude","messages":[]}`),
			want:    true,
		},
		{
			name:    "path=/v1/messages?foo=bar（带 query）",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages?foo=bar"}},
			body:    nil,
			want:    true,
		},
		// 子串匹配防漏点
		{
			name:    "path=/v1/messages-custom 非 Anthropic 派生前缀",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages-custom"}},
			body:    nil,
			want:    false,
		},
		{
			name:    "query 里夹杂 /v1/messages 字样不应触发",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/chat/completions?referer=/v1/messages"}},
			body:    nil,
			want:    false,
		},
		// path 命中 OpenAI
		{
			name:    "path=/v1/chat/completions",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/chat/completions"}},
			body:    []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    false,
		},
		{
			name:    "path=/v1/responses",
			headers: http.Header{"X-Forwarded-Path": []string{"/v1/responses"}},
			body:    []byte(`{"model":"gpt-5.4","input":"hi"}`),
			want:    false,
		},
		// 头部兜底
		{
			name:    "Anthropic-Version 头",
			headers: http.Header{"Anthropic-Version": []string{"2023-06-01"}},
			body:    []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    true,
		},
		// 不再依靠 body 启发——body 有 Anthropic 风味但没 path/header 信号时，默认 OpenAI
		{
			name:    "body 有 top-level system 但无 path/header → 默认 OpenAI",
			headers: nil,
			body:    []byte(`{"model":"x","system":"You are helpful","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
			want:    false,
		},
		{
			name:    "OpenAI chat.completions 无 path/header（之前会被误判，回归用例）",
			headers: nil,
			body:    []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`),
			want:    false,
		},
		{
			name:    "OpenAI vision 带 content block 数组（以前会误判）",
			headers: nil,
			body:    []byte(`{"model":"gpt-4-vision","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"..."}}]}],"max_tokens":4}`),
			want:    false,
		},
		{
			name:    "空 body + 无 headers",
			headers: nil,
			body:    nil,
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &sdk.ForwardRequest{Headers: tc.headers, Body: tc.body}
			if got := isAnthropicRequest(req); got != tc.want {
				t.Errorf("isAnthropicRequest() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyContinuationStateBackfillsPreviousResponseIDForToolOutput(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output":  "ok",
			},
		},
	}

	session := openAISessionResolution{PreviousRespID: "resp_prev"}
	reqBody = applyContinuationState(reqBody, session)
	if got, _ := reqBody["previous_response_id"].(string); got != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev", got)
	}
}

func TestApplyContinuationStateDoesNotBackfillWithToolCallContext(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type":      "function_call",
				"call_id":   "call_1",
				"name":      "lookup",
				"arguments": "{}",
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output":  "ok",
			},
		},
	}

	session := openAISessionResolution{PreviousRespID: "resp_prev"}
	reqBody = applyContinuationState(reqBody, session)
	if got, _ := reqBody["previous_response_id"].(string); got != "" {
		t.Fatalf("expected previous_response_id to stay empty when tool call context is present, got %q", got)
	}
}

func TestApplyContinuationStateDropsUnmatchedToolOutputWithoutPreviousResponseID(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "continue"},
				},
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_missing",
				"output":  "ok",
			},
		},
	}

	reqBody = applyContinuationState(reqBody, openAISessionResolution{})

	input := reqBody["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input length = %d, want 1: %#v", len(input), input)
	}
	if typ := input[0].(map[string]any)["type"]; typ != "message" {
		t.Fatalf("remaining input type = %v, want message", typ)
	}
}

func TestApplyContinuationStatePreservesToolOutputWithPreviousResponseID(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_prev",
				"output":  "ok",
			},
		},
	}

	reqBody = applyContinuationState(reqBody, openAISessionResolution{PreviousRespID: "resp_prev"})

	if got, _ := reqBody["previous_response_id"].(string); got != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev", got)
	}
	input := reqBody["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input length = %d, want 1: %#v", len(input), input)
	}
	if typ := input[0].(map[string]any)["type"]; typ != "function_call_output" {
		t.Fatalf("remaining input type = %v, want function_call_output", typ)
	}
}

func TestApplyContinuationStateTreatsEmptyPreviousResponseIDAsMissing(t *testing.T) {
	reqBody := map[string]any{
		"previous_response_id": "",
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_prev",
				"output":  "ok",
			},
		},
	}

	reqBody = applyContinuationState(reqBody, openAISessionResolution{PreviousRespID: "resp_prev"})

	if got, _ := reqBody["previous_response_id"].(string); got != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev", got)
	}
	input := reqBody["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input length = %d, want 1: %#v", len(input), input)
	}
}

func TestApplyContinuationStateDropsUnmatchedCustomToolOutputWithoutPreviousResponseID(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "custom_tool_call_output",
				"call_id": "call_missing",
				"output":  "ok",
			},
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "continue"},
				},
			},
		},
	}

	reqBody = applyContinuationState(reqBody, openAISessionResolution{})

	input := reqBody["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input length = %d, want 1: %#v", len(input), input)
	}
	if typ := input[0].(map[string]any)["type"]; typ != "message" {
		t.Fatalf("remaining input type = %v, want message", typ)
	}
}

func TestApplyContinuationStatePreservesMatchedCustomToolOutput(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "custom_tool_call",
				"call_id": "call_1",
				"name":    "apply_patch",
				"input":   "patch",
			},
			map[string]any{
				"type":    "custom_tool_call_output",
				"call_id": "call_1",
				"output":  "ok",
			},
		},
	}

	reqBody = applyContinuationState(reqBody, openAISessionResolution{})

	input := reqBody["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input length = %d, want 2: %#v", len(input), input)
	}
	if typ := input[1].(map[string]any)["type"]; typ != "custom_tool_call_output" {
		t.Fatalf("input.1 type = %v, want custom_tool_call_output", typ)
	}
}

func TestBuildSimulatedWSRequestDropsUnmatchedChatToolMessage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"tool","tool_call_id":"call_missing","content":"ok"}]}`)

	got, err := buildSimulatedWSRequest(body, "gpt-5.4", openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildSimulatedWSRequest: %v", err)
	}

	count := 0
	for _, item := range gjson.GetBytes(got, "input").Array() {
		if item.Get("type").String() == "function_call_output" {
			count++
		}
	}
	if count != 0 {
		t.Fatalf("function_call_output count = %d, want 0; body=%s", count, got)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "hi" {
		t.Fatalf("remaining user text = %q, want hi; body=%s", text, got)
	}
}

func TestBuildSimulatedWSRequestPreservesChatToolMessageWithPreviousResponseID(t *testing.T) {
	body := []byte(`{"messages":[{"role":"tool","tool_call_id":"call_prev","content":"ok"}]}`)

	got, err := buildSimulatedWSRequest(body, "gpt-5.4", openAISessionResolution{PreviousRespID: "resp_prev"})
	if err != nil {
		t.Fatalf("buildSimulatedWSRequest: %v", err)
	}

	if previous := gjson.GetBytes(got, "previous_response_id").String(); previous != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev; body=%s", previous, got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "function_call_output" {
		t.Fatalf("input.0.type = %q, want function_call_output; body=%s", typ, got)
	}
}

func TestBuildCodexWSRequestBackfillsPreviousResponseIDAfterResponsesPreprocess(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"function_call_output","call_id":"call_prev","output":"ok"}]}`)
	preprocessed := preprocessRequestBody(body, "gpt-5.4", "/v1/responses")

	if typ := gjson.GetBytes(preprocessed, "input.0.type").String(); typ != "function_call_output" {
		t.Fatalf("preprocess removed tool output: %s", preprocessed)
	}

	got, err := buildCodexWSRequest(preprocessed, "gpt-5.4", openAISessionResolution{PreviousRespID: "resp_prev"})
	if err != nil {
		t.Fatalf("buildCodexWSRequest: %v", err)
	}
	if previous := gjson.GetBytes(got, "previous_response_id").String(); previous != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev; body=%s", previous, got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "function_call_output" {
		t.Fatalf("input.0.type = %q, want function_call_output; body=%s", typ, got)
	}
}

func TestBuildCodexWSRequestDropsEncryptedReplayWhenAddingPreviousResponseID(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest()
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"` + valid + `"},{"type":"compaction_summary","encrypted_content":"summary"},{"type":"function_call_output","call_id":"call_prev","output":"ok"}]}`)
	preprocessed := preprocessRequestBody(body, "gpt-5.4", "/v1/responses")

	got, err := buildCodexWSRequest(preprocessed, "gpt-5.4", openAISessionResolution{PreviousRespID: "resp_prev"})
	if err != nil {
		t.Fatalf("buildCodexWSRequest: %v", err)
	}
	if previous := gjson.GetBytes(got, "previous_response_id").String(); previous != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev; body=%s", previous, got)
	}
	if bytes.Contains([]byte(gjson.GetBytes(got, "input").Raw), []byte("encrypted_content")) {
		t.Fatalf("encrypted replay content was not removed: %s", got)
	}
	if count := gjson.GetBytes(got, "input.#").Int(); count != 1 {
		t.Fatalf("input item count = %d, want 1; body=%s", count, got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "function_call_output" {
		t.Fatalf("input.0.type = %q, want function_call_output; body=%s", typ, got)
	}
}

func TestBuildCodexWSRequestConvertsChatCompletionsMessages(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"be concise"},{"role":"user","content":"hi"}],"stream_options":{"include_usage":true},"max_completion_tokens":32}`)

	got, err := buildCodexWSRequest(body, "gpt-5.5", openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildCodexWSRequest: %v", err)
	}

	if typ := gjson.GetBytes(got, "type").String(); typ != "response.create" {
		t.Fatalf("type = %q, want response.create; body=%s", typ, got)
	}
	if gjson.GetBytes(got, "messages").Exists() {
		t.Fatalf("messages should not be sent to Codex WS upstream: %s", got)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "hi" {
		t.Fatalf("input text = %q, want hi; body=%s", text, got)
	}
	if instructions := gjson.GetBytes(got, "instructions").String(); instructions != "be concise" {
		t.Fatalf("instructions = %q, want be concise; body=%s", instructions, got)
	}
	if gjson.GetBytes(got, "stream_options").Exists() {
		t.Fatalf("stream_options should not be forwarded to Codex WS upstream: %s", got)
	}
	if gjson.GetBytes(got, "max_completion_tokens").Exists() {
		t.Fatalf("max_completion_tokens should not be forwarded to Codex WS upstream: %s", got)
	}
}

func TestBuildCodexWSRequestNormalizesResponseCreateMessages(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.5","previous_response_id":"resp_prev","messages":[{"role":"user","content":"continue"}],"stream_options":{"include_usage":true}}`)

	got, err := buildCodexWSRequest(body, "gpt-5.5", openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildCodexWSRequest: %v", err)
	}

	if gjson.GetBytes(got, "messages").Exists() {
		t.Fatalf("messages should not be sent to Codex WS upstream: %s", got)
	}
	if previous := gjson.GetBytes(got, "previous_response_id").String(); previous != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev; body=%s", previous, got)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "continue" {
		t.Fatalf("input text = %q, want continue; body=%s", text, got)
	}
	if gjson.GetBytes(got, "stream_options").Exists() {
		t.Fatalf("stream_options should not be forwarded to Codex WS upstream: %s", got)
	}
}

func TestBuildWSRequestNormalizesChatBodyIndependentOfCodexHeaders(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)
	cases := []struct {
		name    string
		headers http.Header
	}{
		{
			name:    "codex headers",
			headers: http.Header{"User-Agent": []string{"codex-cli/1.0"}, "originator": []string{"codex_cli_rs"}},
		},
		{
			name:    "plain OpenAI client headers",
			headers: http.Header{"User-Agent": []string{"openai-go/1.0"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &sdk.ForwardRequest{
				Headers: tc.headers,
				Body:    body,
				Model:   "gpt-5.5",
			}
			got, err := (&OpenAIGateway{}).buildWSRequest(req, openAISessionResolution{})
			if err != nil {
				t.Fatalf("buildWSRequest: %v", err)
			}
			if gjson.GetBytes(got, "messages").Exists() {
				t.Fatalf("messages should not depend on request headers: %s", got)
			}
			if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "hi" {
				t.Fatalf("input text = %q, want hi; body=%s", text, got)
			}
		})
	}
}

func TestPreviousResponseNotFoundRecoveryBodyDropsSoftAnchor(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_old","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	got, ok := previousResponseNotFoundRecoveryBody(body)
	if !ok {
		t.Fatalf("expected recovery body")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id was not removed: %s", got)
	}
	if input := gjson.GetBytes(got, "input.0.content.0.text").String(); input != "hi" {
		t.Fatalf("input text = %q, want hi; body=%s", input, got)
	}
}

func TestPreviousResponseNotFoundRecoveryBodyRejectsToolOutputWithoutContext(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_old","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)

	if _, ok := previousResponseNotFoundRecoveryBody(body); ok {
		t.Fatalf("expected recovery to be rejected for tool output without call context")
	}
}

func TestPreviousResponseNotFoundRecoveryBodyAllowsToolOutputWithContext(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_old","input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)

	got, ok := previousResponseNotFoundRecoveryBody(body)
	if !ok {
		t.Fatalf("expected recovery body when tool call context is present")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id was not removed: %s", got)
	}
}

func TestPreviousResponseNotFoundRecoveryBodyAllowsModelBudgetFullContext(t *testing.T) {
	largeText := strings.Repeat("x", 900<<10)
	body := []byte(`{"model":"gpt-5.5","previous_response_id":"resp_old","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + largeText + `"}]}]}`)

	got, ok := previousResponseNotFoundRecoveryBody(body)
	if !ok {
		t.Fatalf("expected recovery body within gpt-5.5 model budget")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id was not removed: %s", got)
	}
}

func TestPreviousResponseNotFoundRecoveryBodyAllowsOversizedFullContext(t *testing.T) {
	largeText := strings.Repeat("x", 1<<20)
	body := []byte(`{"model":"gpt-5.4-mini","previous_response_id":"resp_old","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + largeText + `"}]}]}`)

	got, ok := previousResponseNotFoundRecoveryBody(body)
	if !ok {
		t.Fatalf("expected oversized recovery to be delegated to upstream")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id was not removed: %s", got)
	}
}

func TestPreviousResponseNotFoundRecoveryBodyAllowsCompactionReplayToolOutput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4-mini","previous_response_id":"resp_old","input":[{"type":"compaction","encrypted_content":"summary"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)

	got, ok := previousResponseNotFoundRecoveryBody(body)
	if !ok {
		t.Fatalf("expected compaction replay recovery body")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id was not removed: %s", got)
	}
}

func TestPreviousResponseNotFoundRecoveryBodyDropsEncryptedReasoning(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_old","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"sealed"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)

	got, ok := previousResponseNotFoundRecoveryBody(body)
	if !ok {
		t.Fatalf("expected recovery body")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id was not removed: %s", got)
	}
	if bytes.Contains(got, []byte("encrypted_content")) {
		t.Fatalf("encrypted reasoning content was not removed: %s", got)
	}
	if count := gjson.GetBytes(got, "input.#").Int(); count != 1 {
		t.Fatalf("input item count = %d, want 1; body=%s", count, got)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "continue" {
		t.Fatalf("input text = %q, want continue; body=%s", text, got)
	}
}

func TestFunctionCallOutputRecoveryBodyDropsStalePreviousAndOrphanOutput(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"resp_old","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]},{"type":"function_call_output","call_id":"call_missing","output":"ok"}]}`)

	got, ok := functionCallOutputRecoveryBody(body)
	if !ok {
		t.Fatalf("expected recovery body")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id was not removed: %s", got)
	}
	if count := gjson.GetBytes(got, "input.#").Int(); count != 1 {
		t.Fatalf("input count = %d, want 1; body=%s", count, got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "message" {
		t.Fatalf("input.0.type = %q, want message; body=%s", typ, got)
	}
}

func TestFunctionCallOutputRecoveryBodyPreservesMatchedOutput(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"resp_old","input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)

	got, ok := functionCallOutputRecoveryBody(body)
	if !ok {
		t.Fatalf("expected recovery body")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id was not removed: %s", got)
	}
	if count := gjson.GetBytes(got, "input.#").Int(); count != 2 {
		t.Fatalf("input count = %d, want 2; body=%s", count, got)
	}
	if typ := gjson.GetBytes(got, "input.1.type").String(); typ != "function_call_output" {
		t.Fatalf("input.1.type = %q, want function_call_output; body=%s", typ, got)
	}
}

func TestFunctionCallOutputRecoveryBodyDropsEncryptedReasoning(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"resp_old","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"sealed"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]},{"type":"function_call_output","call_id":"call_missing","output":"ok"}]}`)

	got, ok := functionCallOutputRecoveryBody(body)
	if !ok {
		t.Fatalf("expected recovery body")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id was not removed: %s", got)
	}
	if bytes.Contains(got, []byte("encrypted_content")) {
		t.Fatalf("encrypted reasoning content was not removed: %s", got)
	}
	if count := gjson.GetBytes(got, "input.#").Int(); count != 1 {
		t.Fatalf("input count = %d, want 1; body=%s", count, got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "message" {
		t.Fatalf("input.0.type = %q, want message; body=%s", typ, got)
	}
}

func TestFunctionCallOutputRecoveryBodyDropsCustomToolOrphanOutput(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"resp_old","input":[{"type":"custom_tool_call_output","call_id":"call_missing","output":"ok"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)

	got, ok := functionCallOutputRecoveryBody(body)
	if !ok {
		t.Fatalf("expected recovery body")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id was not removed: %s", got)
	}
	if count := gjson.GetBytes(got, "input.#").Int(); count != 1 {
		t.Fatalf("input count = %d, want 1; body=%s", count, got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "message" {
		t.Fatalf("input.0.type = %q, want message; body=%s", typ, got)
	}
}

func TestPreprocessRequestBodyAppliesDelegatedContinuationRecovery(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_old","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"sealed"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)
	headers := http.Header{airgateContinuationRecoveryHeader: []string{airgateContinuationRecoveryDropPrevious}}

	got := preprocessRequestBody(body, "gpt-5.4", "/v1/responses", headers)

	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id was not removed: %s", got)
	}
	if bytes.Contains(got, []byte("encrypted_content")) {
		t.Fatalf("encrypted reasoning content was not removed: %s", got)
	}
	if count := gjson.GetBytes(got, "input.#").Int(); count != 1 {
		t.Fatalf("input item count = %d, want 1; body=%s", count, got)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "continue" {
		t.Fatalf("input text = %q, want continue; body=%s", text, got)
	}
	if !delegatedContinuationRecoveryApplied(got, headers) {
		t.Fatalf("expected delegated recovery to be marked applied: %s", got)
	}
}

func TestPreprocessRequestBodyDropsEncryptedReplayFromAnchoredContinuation(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_old","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"sealed"},{"type":"compaction","encrypted_content":"compact"},{"type":"compaction_summary","encrypted_content":"summary"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)

	got := preprocessRequestBody(body, "gpt-5.4", "/v1/responses")

	if previous := gjson.GetBytes(got, "previous_response_id").String(); previous != "resp_old" {
		t.Fatalf("previous_response_id = %q, want resp_old; body=%s", previous, got)
	}
	if bytes.Contains([]byte(gjson.GetBytes(got, "input").Raw), []byte("encrypted_content")) {
		t.Fatalf("encrypted replay content was not removed: %s", got)
	}
	if count := gjson.GetBytes(got, "input.#").Int(); count != 1 {
		t.Fatalf("input item count = %d, want 1; body=%s", count, got)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "continue" {
		t.Fatalf("input text = %q, want continue; body=%s", text, got)
	}
}

func TestPreprocessRequestBodySkipsDelegatedContinuationRecoveryForUnsafeToolOutput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_old","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	headers := http.Header{airgateContinuationRecoveryHeader: []string{airgateContinuationRecoveryDropPrevious}}

	got := preprocessRequestBody(body, "gpt-5.4", "/v1/responses", headers)

	if previous := gjson.GetBytes(got, "previous_response_id").String(); previous != "resp_old" {
		t.Fatalf("previous_response_id = %q, want resp_old; body=%s", previous, got)
	}
	if delegatedContinuationRecoveryApplied(got, headers) {
		t.Fatalf("unsafe tool output must not be treated as delegated full replay: %s", got)
	}
}

func TestDelegatedContinuationRecoveryAppliedRejectsHeaderOnlyReplay(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)
	headers := http.Header{airgateContinuationRecoveryHeader: []string{airgateContinuationRecoveryDropPrevious}}

	if delegatedContinuationRecoveryApplied(body, headers) {
		t.Fatalf("header-only delegated recovery must not be treated as full replay")
	}
}

func TestPreprocessRequestBodyRejectsForgedDelegatedRecoveryMarker(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest()
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"` + valid + `"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)
	headers := http.Header{
		airgateContinuationRecoveryHeader:  []string{airgateContinuationRecoveryDropPrevious},
		airgateContinuationRecoveryApplied: []string{"true"},
	}

	got := preprocessRequestBody(body, "gpt-5.4", "/v1/responses", headers)

	if gotEnc := gjson.GetBytes(got, "input.0.encrypted_content").String(); gotEnc != valid {
		t.Fatalf("encrypted_content = %q, want valid replay content; body=%s", gotEnc, got)
	}
	if gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("reasoning id was not removed: %s", got)
	}
	if marker := headers.Get(airgateContinuationRecoveryApplied); marker != "" {
		t.Fatalf("forged delegated recovery marker was not cleared: %q", marker)
	}
	if delegatedContinuationRecoveryApplied(got, headers) {
		t.Fatalf("body without removed previous_response_id must not be treated as full replay: %s", got)
	}
}

func TestPreprocessRequestBodyCompactDeletesStreamOnly(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","stream":false,"input":"hello"}`)

	got := preprocessRequestBody(body, "gpt-5.4-openai-compact", "/v1/responses/compact")

	if model := gjson.GetBytes(got, "model").String(); model != "gpt-5.4-openai-compact" {
		t.Fatalf("compact model = %q, want gpt-5.4-openai-compact; body=%s", model, got)
	}
	if gjson.GetBytes(got, "stream").Exists() {
		t.Fatalf("stream should be removed for compact request: %s", got)
	}
	if input := gjson.GetBytes(got, "input"); input.Type != gjson.String || input.String() != "hello" {
		t.Fatalf("compact input should stay unchanged, got %s in %s", input.Raw, got)
	}
	if gjson.GetBytes(got, "store").Exists() {
		t.Fatalf("store should not be injected for compact request: %s", got)
	}
}

func TestPreprocessRequestBodyCompactUsesSelectedBaseModel(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5-openai-compact","stream":false,"input":"hello"}`)

	got := preprocessRequestBody(body, "gpt-5.5", "/v1/responses/compact")

	if model := gjson.GetBytes(got, "model").String(); model != "gpt-5.5" {
		t.Fatalf("compact model = %q, want gpt-5.5; body=%s", model, got)
	}
}

func TestNormalizePromptCacheKeyForUpstreamHashesLongKey(t *testing.T) {
	key := strings.Repeat("k", maxUpstreamPromptCacheKeyLength+6)
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","input":"hi","prompt_cache_key":%q}`, key))

	got := normalizePromptCacheKeyForUpstream(body)
	normalized := gjson.GetBytes(got, "prompt_cache_key").String()

	if normalized == key {
		t.Fatalf("expected long prompt_cache_key to be normalized")
	}
	if len(normalized) != maxUpstreamPromptCacheKeyLength {
		t.Fatalf("prompt_cache_key length = %d, want %d; body=%s", len(normalized), maxUpstreamPromptCacheKeyLength, got)
	}
	if normalized != upstreamPromptCacheKey(key) {
		t.Fatalf("prompt_cache_key = %q, want %q", normalized, upstreamPromptCacheKey(key))
	}
}

func TestNormalizePromptCacheKeyForUpstreamKeepsShortKey(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hi","prompt_cache_key":"cache-key-123"}`)

	got := normalizePromptCacheKeyForUpstream(body)

	if normalized := gjson.GetBytes(got, "prompt_cache_key").String(); normalized != "cache-key-123" {
		t.Fatalf("prompt_cache_key = %q, want cache-key-123; body=%s", normalized, got)
	}
}

func TestApplySessionFieldsHashesLongPromptCacheKey(t *testing.T) {
	key := strings.Repeat("s", maxUpstreamPromptCacheKeyLength+6)
	reqData := applySessionFields(map[string]any{}, openAISessionResolution{PromptCacheKey: key})

	normalized, _ := reqData["prompt_cache_key"].(string)
	if normalized != upstreamPromptCacheKey(key) {
		t.Fatalf("prompt_cache_key = %q, want %q", normalized, upstreamPromptCacheKey(key))
	}
}

func TestInjectAnthropicPromptCacheKeyHashesLongKey(t *testing.T) {
	key := strings.Repeat("a", maxUpstreamPromptCacheKeyLength+6)
	body := []byte(`{"model":"gpt-5.5","input":[]}`)

	got := injectAnthropicPromptCacheKey(body, anthropicStrategyGenericAPIKey, openAISessionResolution{PromptCacheKey: key})

	if normalized := gjson.GetBytes(got, "prompt_cache_key").String(); normalized != upstreamPromptCacheKey(key) {
		t.Fatalf("prompt_cache_key = %q, want %q; body=%s", normalized, upstreamPromptCacheKey(key), got)
	}
}

func TestInjectAnthropicPromptCacheKeyNormalizesExistingOAuthKey(t *testing.T) {
	key := strings.Repeat("o", maxUpstreamPromptCacheKeyLength+6)
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","input":[],"prompt_cache_key":%q}`, key))

	got := injectAnthropicPromptCacheKey(body, anthropicStrategyOAuth, openAISessionResolution{})

	if normalized := gjson.GetBytes(got, "prompt_cache_key").String(); normalized != upstreamPromptCacheKey(key) {
		t.Fatalf("prompt_cache_key = %q, want %q; body=%s", normalized, upstreamPromptCacheKey(key), got)
	}
}

func TestPluginRouteDefinitionsIncludesResponsesCompact(t *testing.T) {
	routes := PluginRouteDefinitions()
	want := map[string]bool{
		"POST /v1/responses/compact": false,
		"POST /responses/compact":    false,
	}
	for _, route := range routes {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Fatalf("route %s not registered", key)
		}
	}
}

func TestNormalizeOpenAIServiceTier_FastIsInvalid(t *testing.T) {
	if got := normalizeOpenAIServiceTier("fast"); got != "" {
		t.Fatalf("normalizeOpenAIServiceTier(fast) = %q, want empty", got)
	}
}

func TestNormalizeOpenAIWireServiceTier_FastIsInvalid(t *testing.T) {
	if got := normalizeOpenAIWireServiceTier("fast"); got != "" {
		t.Fatalf("normalizeOpenAIWireServiceTier(fast) = %q, want empty", got)
	}
}

func TestEnsureResponsesDefaultsWithTier_FastIgnored(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hi"}`)
	result := ensureResponsesDefaultsWithTier(body, "fast")

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := payload["service_tier"]; ok {
		t.Fatalf("service_tier should be omitted for fast, got %v", payload["service_tier"])
	}
}

func TestEnsureResponsesDefaultsNormalizesMaxReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hi","reasoning_effort":"max"}`)
	result := ensureResponsesDefaultsWithTier(body, "")

	if got := gjson.GetBytes(result, "reasoning.effort").String(); got != "max" {
		t.Fatalf("reasoning.effort = %q, want max; body=%s", got, result)
	}
}

func TestEnsureResponsesDefaultsNormalizesExistingMaxReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hi","reasoning":{"effort":"maximum"}}`)
	result := ensureResponsesDefaultsWithTier(body, "")

	if got := gjson.GetBytes(result, "reasoning.effort").String(); got != "max" {
		t.Fatalf("reasoning.effort = %q, want max; body=%s", got, result)
	}
}

func TestEnsureResponsesDefaultsUsesOutputConfigMaxEffort(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hi","output_config":{"effort":"max"}}`)
	result := ensureResponsesDefaultsWithTier(body, "")

	if got := gjson.GetBytes(result, "reasoning.effort").String(); got != "max" {
		t.Fatalf("reasoning.effort = %q, want max; body=%s", got, result)
	}
	if gjson.GetBytes(result, "output_config").Exists() {
		t.Fatalf("output_config should be stripped from upstream body: %s", result)
	}
}

func TestEnsureResponsesDefaultsPreservesReasoningEffortNone(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hi","reasoning_effort":"none"}`)
	result := ensureResponsesDefaultsWithTier(body, "")

	if got := gjson.GetBytes(result, "reasoning.effort").String(); got != "none" {
		t.Fatalf("reasoning.effort = %q, want none; body=%s", got, result)
	}
}

func TestOpenAIReasoningHintMatchesSpacedKeys(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hi","reasoning" : {"effort" : "maximum"}}`)

	if !hasOpenAIReasoningDefaultsHint(body) {
		t.Fatal("expected reasoning defaults hint")
	}
	if got := openAIReasoningEffortFromRequest(body); got != "max" {
		t.Fatalf("openAIReasoningEffortFromRequest = %q, want max", got)
	}
}

func TestOpenAIReasoningHintIgnoresEscapedJSONInStringValue(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"{\"reasoning\":{\"effort\":\"max\"},\"output_config\":{\"effort\":\"max\"},\"reasoning_effort\":\"max\"}"}`)

	if hasOpenAIReasoningDefaultsHint(body) {
		t.Fatal("reasoning defaults hint should ignore escaped JSON in string values")
	}
	if hasOpenAIReasoningEffortHint(body) {
		t.Fatal("reasoning effort hint should ignore escaped JSON in string values")
	}
}

// ---------------------------------------------------------------------------
// normalizeOpenAIReasoningEffort — 所有别名、大小写变体、分隔符变体、边界
// ---------------------------------------------------------------------------

func TestNormalizeOpenAIReasoningEffort_AllAliases(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// 规范值 — Phase 1 精确命中
		{"none", "none"},
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},

		// xhigh / max 精确值和别名 — Phase 1 精确命中
		{"xhigh", "xhigh"},
		{"extrahigh", "xhigh"},
		{"veryhigh", "xhigh"},
		{"max", "max"},
		{"maximum", "max"},
		{"ultra", "ultra"},

		// 禁用思考别名 — Phase 1 精确命中
		{"minimal", "none"},
		{"min", "none"},
		{"off", "none"},
		{"disabled", "none"},

		// 大小写变体 — Phase 2 ToLower
		{"None", "none"},
		{"NONE", "none"},
		{"Low", "low"},
		{"Medium", "medium"},
		{"High", "high"},
		{"Xhigh", "xhigh"},
		{"XHIGH", "xhigh"},
		{"Max", "max"},
		{"MAX", "max"},
		{"Maximum", "max"},
		{"ExtraHigh", "xhigh"},
		{"VeryHigh", "xhigh"},
		{"Ultra", "ultra"},
		{"Minimal", "none"},
		{"Min", "none"},
		{"Off", "none"},
		{"Disabled", "none"},
		{"DISABLED", "none"},

		// 带下划线变体 — Phase 2 去 _
		{"extra_high", "xhigh"},
		{"very_high", "xhigh"},
		{"EXTRA_HIGH", "xhigh"},

		// 带空格变体 — Phase 2 去空格
		{"extra high", "xhigh"},
		{"very high", "xhigh"},

		// 前后空白
		{"  none  ", "none"},
		{"  high\t", "high"},
		{"\tmax\n", "max"},

		// 不可识别 — 原样返回 trimmed
		{"unknown", "unknown"},
		{"extreme", "extreme"},

		// 空 / 纯空白
		{"", ""},
		{"   ", ""},
		{"\t\n", ""},
	}
	for _, tc := range cases {
		if got := normalizeOpenAIReasoningEffort(tc.input); got != tc.want {
			t.Errorf("normalizeOpenAIReasoningEffort(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestOpenAIWireReasoningEffortClampsUnsupportedLevels(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"none", "none"},
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"max", "xhigh"},
		{"maximum", "xhigh"},
		{"ultra", "xhigh"},
		{"unknown", "unknown"},
	}
	for _, tc := range cases {
		if got := openAIWireReasoningEffort(tc.input, openAIWireReasoningSupport{}); got != tc.want {
			t.Errorf("openAIWireReasoningEffort(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestOpenAIWireReasoningEffortAllowsDeclaredExtendedLevels(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"max", "max"},
		{"maximum", "max"},
		{"ultra", "ultra"},
		{"xhigh", "xhigh"},
		{"unknown", "unknown"},
	}
	for _, tc := range cases {
		if got := openAIWireReasoningEffort(tc.input, openAIWireReasoningSupport{Max: true, Ultra: true}); got != tc.want {
			t.Errorf("openAIWireReasoningEffort(%q, support=max+ultra) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestApplyOpenAIWireReasoningEffortClampsUnsupportedLevels(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"max"},"reasoning_effort":"ultra","output_config":{"effort":"maximum"}}`)
	result := applyOpenAIWireReasoningEffort(body, "gpt-5.5")

	if got := gjson.GetBytes(result, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh; body=%s", got, result)
	}
	if got := gjson.GetBytes(result, "reasoning_effort").String(); got != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want xhigh; body=%s", got, result)
	}
	if got := gjson.GetBytes(result, "output_config.effort").String(); got != "xhigh" {
		t.Fatalf("output_config.effort = %q, want xhigh; body=%s", got, result)
	}
}

func TestApplyOpenAIWireReasoningEffortClampsLunaUltra(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"max"},"reasoning_effort":"ultra"}`)
	result := applyOpenAIWireReasoningEffort(body, "gpt-5.6-luna")

	if got := gjson.GetBytes(result, "reasoning.effort").String(); got != "max" {
		t.Fatalf("reasoning.effort = %q, want max; body=%s", got, result)
	}
	if got := gjson.GetBytes(result, "reasoning_effort").String(); got != "max" {
		t.Fatalf("reasoning_effort = %q, want max; body=%s", got, result)
	}
}

func TestApplyOpenAIWireReasoningEffortMapsGPT56UltraAtWireBoundary(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"max"},"reasoning_effort":"ultra","output_config":{"effort":"maximum"}}`)
	result := applyOpenAIWireReasoningEffort(body, "gpt-5.6-sol")

	if got := gjson.GetBytes(result, "reasoning.effort").String(); got != "max" {
		t.Fatalf("reasoning.effort = %q, want max; body=%s", got, result)
	}
	if got := gjson.GetBytes(result, "reasoning_effort").String(); got != "max" {
		t.Fatalf("reasoning_effort = %q, want max; body=%s", got, result)
	}
	if got := gjson.GetBytes(result, "output_config.effort").String(); got != "max" {
		t.Fatalf("output_config.effort = %q, want max; body=%s", got, result)
	}
}

func TestOpenAIWireReasoningSupportForModelRecognizesAliases(t *testing.T) {
	cases := []struct {
		model string
		want  openAIWireReasoningSupport
	}{
		{"gpt-5.6-sol", openAIWireReasoningSupport{Max: true}},
		{"openai/gpt-5.6-terra", openAIWireReasoningSupport{Max: true}},
		{"oai/gpt-5.6-luna-openai-compact", openAIWireReasoningSupport{Max: true}},
		{"gpt-5.5", openAIWireReasoningSupport{}},
	}
	for _, tc := range cases {
		if got := openAIWireReasoningSupportForModel(tc.model); got != tc.want {
			t.Fatalf("openAIWireReasoningSupportForModel(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// hasJSONKeyToken — JSON key 精确匹配 vs string value / 转义
// ---------------------------------------------------------------------------

func TestHasJSONKeyToken_SimpleKey(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"high"}}`)
	if !hasJSONKeyToken(body, jsonReasoningKey) {
		t.Fatal(`expected "reasoning" key to be found`)
	}
}

func TestHasJSONKeyToken_KeyWithSpaceBeforeColon(t *testing.T) {
	body := []byte(`{"reasoning" : {"effort":"high"}}`)
	if !hasJSONKeyToken(body, jsonReasoningKey) {
		t.Fatal(`expected "reasoning" key with space before colon to be found`)
	}
}

func TestHasJSONKeyToken_KeyWithNewlineBeforeColon(t *testing.T) {
	body := []byte("{\"reasoning\"\n: {\"effort\":\"high\"}}")
	if !hasJSONKeyToken(body, jsonReasoningKey) {
		t.Fatal(`expected "reasoning" key with newline before colon to be found`)
	}
}

func TestHasJSONKeyToken_KeyWithTabBeforeColon(t *testing.T) {
	body := []byte("{\"reasoning\"\t: {\"effort\":\"high\"}}")
	if !hasJSONKeyToken(body, jsonReasoningKey) {
		t.Fatal(`expected "reasoning" key with tab before colon to be found`)
	}
}

func TestHasJSONKeyToken_MultipleWhitespaceBeforeColon(t *testing.T) {
	body := []byte("{\"reasoning\"  \t\n: {\"effort\":\"high\"}}")
	if !hasJSONKeyToken(body, jsonReasoningKey) {
		t.Fatal(`expected "reasoning" key with mixed whitespace before colon`)
	}
}

func TestHasJSONKeyToken_StringValueIgnored(t *testing.T) {
	// "reasoning" 作为 string value 出现，后面跟的不是 :
	body := []byte(`{"input":"reasoning"}`)
	if hasJSONKeyToken(body, jsonReasoningKey) {
		t.Fatal(`"reasoning" in string value should not be treated as key`)
	}
}

func TestHasJSONKeyToken_StringValueWithColonIgnored(t *testing.T) {
	// "reasoning: x" 整体是 string value 的一部分
	body := []byte(`{"input":"reasoning: xyz"}`)
	if hasJSONKeyToken(body, jsonReasoningKey) {
		t.Fatal(`"reasoning" inside a string value followed by colon should not match`)
	}
}

func TestHasJSONKeyToken_EscapedQuoteInStringValue(t *testing.T) {
	body := []byte(`{"input":"{\"reasoning\":{\"effort\":\"max\"}}"}`)
	if hasJSONKeyToken(body, jsonReasoningKey) {
		t.Fatal(`escaped \"reasoning\" inside string value should be ignored`)
	}
}

func TestHasJSONKeyToken_EscapedBackslashBeforeKey(t *testing.T) {
	// \\" (偶数个反斜杠 = 一个字面反斜杠 + 未转义引号)
	// 在 JSON 中: "input": "\\"reasoning\": ..."
	// Go 字面量: `{"input":"\\\"reasoning\\\":{}}`
	body := []byte(`{"input":"\\\"reasoning\\\":{}}"}`)
	if hasJSONKeyToken(body, jsonReasoningKey) {
		t.Fatal(`key preceded by even backslashes in string value should not match`)
	}
}

func TestHasJSONKeyToken_FirstStringValueSecondKey(t *testing.T) {
	// 第一个 "reasoning" 在 string value 中，第二个是真正的 key
	body := []byte(`{"input":"reasoning","reasoning":{"effort":"high"}}`)
	if !hasJSONKeyToken(body, jsonReasoningKey) {
		t.Fatal(`second "reasoning" as a real JSON key should be found`)
	}
}

func TestHasJSONKeyToken_EmptyBody(t *testing.T) {
	if hasJSONKeyToken(nil, jsonReasoningKey) {
		t.Fatal("nil body should return false")
	}
	if hasJSONKeyToken([]byte{}, jsonReasoningKey) {
		t.Fatal("empty body should return false")
	}
}

func TestHasJSONKeyToken_NoMatch(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hi"}`)
	if hasJSONKeyToken(body, jsonReasoningKey) {
		t.Fatal("body without target key should return false")
	}
}

func TestHasJSONKeyToken_EffortKey(t *testing.T) {
	body := []byte(`{"effort":"high"}`)
	if !hasJSONKeyToken(body, jsonEffortKey) {
		t.Fatal(`expected "effort" key to be found`)
	}
}

func TestHasJSONKeyToken_OutputConfigKey(t *testing.T) {
	body := []byte(`{"output_config":{"effort":"max"}}`)
	if !hasJSONKeyToken(body, jsonOutputConfigKey) {
		t.Fatal(`expected "output_config" key to be found`)
	}
}

func TestHasJSONKeyToken_ReasoningEffortKey(t *testing.T) {
	body := []byte(`{"reasoning_effort":"high"}`)
	if !hasJSONKeyToken(body, jsonReasoningEffortKey) {
		t.Fatal(`expected "reasoning_effort" key to be found`)
	}
}

// ---------------------------------------------------------------------------
// hasOpenAIReasoningEffortHint / hasOpenAIReasoningDefaultsHint
// ---------------------------------------------------------------------------

func TestHasOpenAIReasoningEffortHint_Positive(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"reasoning_effort flat", []byte(`{"reasoning_effort":"high"}`)},
		{"output_config", []byte(`{"output_config":{"effort":"max"}}`)},
		{"reasoning + effort", []byte(`{"reasoning":{"effort":"low"}}`)},
		{"all three", []byte(`{"reasoning":{"effort":"low"},"reasoning_effort":"high","output_config":{"effort":"max"}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !hasOpenAIReasoningEffortHint(tc.body) {
				t.Fatalf("expected hint, body=%s", tc.body)
			}
		})
	}
}

func TestHasOpenAIReasoningEffortHint_Negative(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"no reasoning fields", []byte(`{"model":"gpt-5.5","input":"hi"}`)},
		{"reasoning without effort", []byte(`{"reasoning":{"summary":"auto"}}`)},
		{"escaped JSON in string value", []byte(`{"input":"{\"reasoning\":{\"effort\":\"max\"},\"output_config\":{\"effort\":\"max\"},\"reasoning_effort\":\"max\"}"}`)},
		{"effort as string value", []byte(`{"input":"effort"}`)},
		{"reasoning in string value", []byte(`{"input":"my reasoning about effort"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if hasOpenAIReasoningEffortHint(tc.body) {
				t.Fatalf("expected no hint, body=%s", tc.body)
			}
		})
	}
}

func TestHasOpenAIReasoningDefaultsHint_Positive(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"reasoning key", []byte(`{"reasoning":{"summary":"auto"}}`)},
		{"reasoning_effort key", []byte(`{"reasoning_effort":"high"}`)},
		{"output_config key", []byte(`{"output_config":{}}`)},
		{"reasoning + effort", []byte(`{"reasoning":{"effort":"low"}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !hasOpenAIReasoningDefaultsHint(tc.body) {
				t.Fatalf("expected defaults hint, body=%s", tc.body)
			}
		})
	}
}

func TestHasOpenAIReasoningDefaultsHint_Negative(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"no reasoning fields", []byte(`{"model":"gpt-5.5","input":"hi"}`)},
		{"escaped JSON in string value", []byte(`{"input":"{\"reasoning\":{},\"output_config\":{},\"reasoning_effort\":\"\"}"}`)},
		{"reasoning as string value not key", []byte(`{"input":"reasoning"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if hasOpenAIReasoningDefaultsHint(tc.body) {
				t.Fatalf("expected no defaults hint, body=%s", tc.body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// openAIReasoningEffortFromRequest — 三路径优先级 + FromRequestAfterHint
// ---------------------------------------------------------------------------

func TestOpenAIReasoningEffortFromRequest_Priority(t *testing.T) {
	// reasoning.effort > reasoning_effort > output_config.effort
	t.Run("reasoning.effort over reasoning_effort", func(t *testing.T) {
		body := []byte(`{"reasoning":{"effort":"high"},"reasoning_effort":"low"}`)
		if got := openAIReasoningEffortFromRequest(body); got != "high" {
			t.Fatalf("got %q, want high (reasoning.effort should win)", got)
		}
	})
	t.Run("reasoning.effort over output_config", func(t *testing.T) {
		body := []byte(`{"reasoning":{"effort":"medium"},"output_config":{"effort":"max"}}`)
		if got := openAIReasoningEffortFromRequest(body); got != "medium" {
			t.Fatalf("got %q, want medium (reasoning.effort should win)", got)
		}
	})
	t.Run("reasoning_effort over output_config", func(t *testing.T) {
		body := []byte(`{"reasoning_effort":"low","output_config":{"effort":"max"}}`)
		if got := openAIReasoningEffortFromRequest(body); got != "low" {
			t.Fatalf("got %q, want low (reasoning_effort should win)", got)
		}
	})
}

func TestOpenAIReasoningEffortFromRequest_AllPaths(t *testing.T) {
	t.Run("reasoning.effort path", func(t *testing.T) {
		body := []byte(`{"reasoning":{"effort":"high"}}`)
		if got := openAIReasoningEffortFromRequest(body); got != "high" {
			t.Fatalf("got %q, want high", got)
		}
	})
	t.Run("reasoning_effort path", func(t *testing.T) {
		body := []byte(`{"reasoning_effort":"low"}`)
		if got := openAIReasoningEffortFromRequest(body); got != "low" {
			t.Fatalf("got %q, want low", got)
		}
	})
	t.Run("output_config.effort path", func(t *testing.T) {
		body := []byte(`{"output_config":{"effort":"max"}}`)
		if got := openAIReasoningEffortFromRequest(body); got != "max" {
			t.Fatalf("got %q, want max", got)
		}
	})
	t.Run("no reasoning at all", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.5","input":"hi"}`)
		if got := openAIReasoningEffortFromRequest(body); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func TestOpenAIReasoningEffortFromRequest_NormalizesAliases(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"max → max", []byte(`{"reasoning_effort":"max"}`), "max"},
		{"maximum → max", []byte(`{"reasoning_effort":"maximum"}`), "max"},
		{"ultra → ultra", []byte(`{"reasoning_effort":"ultra"}`), "ultra"},
		{"min → none", []byte(`{"reasoning":{"effort":"min"}}`), "none"},
		{"off → none", []byte(`{"reasoning":{"effort":"off"}}`), "none"},
		{"disabled → none", []byte(`{"output_config":{"effort":"disabled"}}`), "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openAIReasoningEffortFromRequest(tc.body); got != tc.want {
				t.Fatalf("got %q, want %q; body=%s", got, tc.want, tc.body)
			}
		})
	}
}

func TestOpenAIReasoningEffortFromRequestAfterHint(t *testing.T) {
	// AfterHint 不检查 hint 直接解析 — 用于 hint 已检查过的热路径
	t.Run("reasoning.effort found", func(t *testing.T) {
		body := []byte(`{"reasoning":{"effort":"high"}}`)
		if got := openAIReasoningEffortFromRequestAfterHint(body); got != "high" {
			t.Fatalf("got %q, want high", got)
		}
	})
	t.Run("empty body", func(t *testing.T) {
		if got := openAIReasoningEffortFromRequestAfterHint([]byte{}); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

// ---------------------------------------------------------------------------
// ensureResponsesDefaultsWithTier — 集成：已存在的 reasoning.effort 也被标准化
// ---------------------------------------------------------------------------

func TestEnsureResponsesDefaultsNormalizesExistingReasoningEffortLowcaseAlias(t *testing.T) {
	tests := []struct {
		name       string
		body       []byte
		wantEffort string
	}{
		{"reasoning.effort extrahigh → xhigh", []byte(`{"model":"gpt-5.5","input":"hi","reasoning":{"effort":"extrahigh"}}`), "xhigh"},
		{"reasoning.effort veryhigh → xhigh", []byte(`{"model":"gpt-5.5","input":"hi","reasoning":{"effort":"veryhigh"}}`), "xhigh"},
		{"reasoning.effort min → none", []byte(`{"model":"gpt-5.5","input":"hi","reasoning":{"effort":"min"}}`), "none"},
		{"reasoning.effort disabled → none", []byte(`{"model":"gpt-5.5","input":"hi","reasoning":{"effort":"disabled"}}`), "none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := ensureResponsesDefaultsWithTier(tc.body, "")
			if got := gjson.GetBytes(result, "reasoning.effort").String(); got != tc.wantEffort {
				t.Fatalf("reasoning.effort = %q, want %q; body=%s", got, tc.wantEffort, result)
			}
		})
	}
}

func TestEnsureResponsesDefaultsPreservesUltraUntilWireBoundary(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":"hi","reasoning":{"effort":"ultra"}}`)
	result := ensureResponsesDefaultsWithTier(body, "")

	if got := gjson.GetBytes(result, "reasoning.effort").String(); got != "ultra" {
		t.Fatalf("reasoning.effort = %q, want ultra; body=%s", got, result)
	}
}

func TestEnsureResponsesDefaultsNoopsWhenNoReasoningFields(t *testing.T) {
	// 没有 reasoning 相关字段时，不应该注入 reasoning
	body := []byte(`{"model":"gpt-5.5","input":"hi"}`)
	result := ensureResponsesDefaultsWithTier(body, "")
	if gjson.GetBytes(result, "reasoning").Exists() {
		t.Fatalf("reasoning should not be injected when no hint; body=%s", result)
	}
	// 但 include 应该仍然被设置
	if include := gjson.GetBytes(result, "include"); !include.Exists() {
		t.Fatal("include should still be set even without reasoning")
	}
}

func TestApplyOpenAIWireServiceTier_FastRemoved(t *testing.T) {
	result := applyOpenAIWireServiceTier([]byte(`{"model":"gpt-5.5","input":"hi","service_tier":"fast"}`))

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := payload["service_tier"]; ok {
		t.Fatalf("service_tier should be removed for fast, got %v", payload["service_tier"])
	}
}

func TestPreprocessRequestBody_PreservesConversationImageDataURLs(t *testing.T) {
	imageRef := largeConversationImageDataURL(t)
	cases := []struct {
		name       string
		path       string
		body       []byte
		resultPath string
	}{
		{
			name:       "chat completions",
			path:       "/v1/chat/completions",
			body:       []byte(fmt.Sprintf(`{"model":"gpt-5.4","messages":[{"role":"user","content":[{"type":"text","text":"描述图片"},{"type":"image_url","image_url":{"url":%q}}]}]}`, imageRef)),
			resultPath: "messages.0.content.1.image_url.url",
		},
		{
			name:       "responses input",
			path:       "/v1/responses",
			body:       []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"描述图片"},{"type":"input_image","image_url":%q}]}]}`, imageRef)),
			resultPath: "input.0.content.1.image_url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := preprocessRequestBody(tc.body, "gpt-5.4", tc.path)
			if gotImage := gjson.GetBytes(got, tc.resultPath).String(); gotImage != imageRef {
				t.Fatalf("conversation image should stay unchanged, got %.32q", gotImage)
			}
		})
	}
}

func largeConversationImageDataURL(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	for y := 0; y < 1024; y++ {
		for x := 0; x < 1024; x++ {
			v := uint32(x)*1103515245 + uint32(y)*12345
			img.SetRGBA(x, y, color.RGBA{R: uint8(v), G: uint8(v >> 8), B: uint8(v >> 16), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&buf, img); err != nil {
		t.Fatalf("png encode failed: %v", err)
	}
	ref := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	if n := len(decodeDataURLBytes(t, ref)); n <= maxResponsesInputImageBytes {
		t.Fatalf("测试图片过小：%d <= %d", n, maxResponsesInputImageBytes)
	}
	return ref
}

func decodeDataURLBytes(t *testing.T, ref string) []byte {
	t.Helper()
	comma := strings.IndexByte(ref, ',')
	if comma < 0 {
		t.Fatalf("data URL 缺少逗号：%.32q", ref)
	}
	raw := ref[comma+1:]
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(raw)
		if err != nil {
			t.Fatalf("base64 解码失败：%v", err)
		}
	}
	return data
}

func TestWrapAsResponsesAPIPreservesChatImageURL(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)
	result, err := wrapAsResponsesAPI(body, "gpt-5.4")
	if err != nil {
		t.Fatalf("wrapAsResponsesAPI: %v", err)
	}

	content := gjson.GetBytes(result, "input.0.content")
	if got := content.Get("0.type").String(); got != "input_text" {
		t.Fatalf("first content type = %q, want input_text", got)
	}
	if got := content.Get("1.type").String(); got != "input_image" {
		t.Fatalf("second content type = %q, want input_image", got)
	}
	if got := content.Get("1.image_url").String(); got != "data:image/png;base64,AAA" {
		t.Fatalf("image_url = %q", got)
	}
}

func TestWrapAsResponsesAPIToolResultOutputIsString(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"sunny"}]}]}`)
	result, err := wrapAsResponsesAPI(body, "gpt-5.4")
	if err != nil {
		t.Fatalf("wrapAsResponsesAPI: %v", err)
	}

	output := gjson.GetBytes(result, "input.1.output")
	if output.Type != gjson.String {
		t.Fatalf("function_call_output.output type = %v, want string; body=%s", output.Type, result)
	}
	if output.String() != "sunny" {
		t.Fatalf("function_call_output.output = %q, want sunny", output.String())
	}
}

func TestWrapAsResponsesAPIDoesNotRequireToolsWhenToolsMissing(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"sunny"}]}`)
	result, err := wrapAsResponsesAPI(body, "gpt-5.4")
	if err != nil {
		t.Fatalf("wrapAsResponsesAPI: %v", err)
	}

	if gjson.GetBytes(result, "tool_choice").Exists() {
		t.Fatalf("tool_choice should not be forwarded without tools: %s", result)
	}
	if gjson.GetBytes(result, "tools").Exists() {
		t.Fatalf("tools should stay absent when client did not provide tools: %s", result)
	}
}

func TestWrapAsResponsesAPIRequiresToolsOnlyWhenToolsPresent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"sunny"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`)
	result, err := wrapAsResponsesAPI(body, "gpt-5.4")
	if err != nil {
		t.Fatalf("wrapAsResponsesAPI: %v", err)
	}

	if tools := gjson.GetBytes(result, "tools.#").Int(); tools != 1 {
		t.Fatalf("tools count = %d, want 1; body=%s", tools, result)
	}
	if choice := gjson.GetBytes(result, "tool_choice").String(); choice != "required" {
		t.Fatalf("tool_choice = %q, want required; body=%s", choice, result)
	}
}

func TestWrapAsResponsesAPIDropsExplicitToolChoiceWithoutTools(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`)
	result, err := wrapAsResponsesAPI(body, "gpt-5.4")
	if err != nil {
		t.Fatalf("wrapAsResponsesAPI: %v", err)
	}

	if gjson.GetBytes(result, "tool_choice").Exists() {
		t.Fatalf("tool_choice should be dropped without tools: %s", result)
	}
}

func TestEnsureResponsesDefaultsDropsToolControlWithoutTools(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"parallel_tool_calls":true,"tool_choice":"required"}`)
	result := ensureResponsesDefaultsWithTier(body, "")

	if gjson.GetBytes(result, "tool_choice").Exists() {
		t.Fatalf("tool_choice should be removed without tools: %s", result)
	}
	if gjson.GetBytes(result, "parallel_tool_calls").Exists() {
		t.Fatalf("parallel_tool_calls should be removed without tools: %s", result)
	}
}

func TestWrapAsResponsesAPIUsesOutputConfigMaxEffort(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"max"}}`)
	result, err := wrapAsResponsesAPI(body, "gpt-5.4")
	if err != nil {
		t.Fatalf("wrapAsResponsesAPI: %v", err)
	}

	if got := gjson.GetBytes(result, "reasoning.effort").String(); got != "max" {
		t.Fatalf("reasoning.effort = %q, want max; body=%s", got, result)
	}
	if gjson.GetBytes(result, "output_config").Exists() {
		t.Fatalf("output_config should not be forwarded: %s", result)
	}
}

func TestNormalizeResponsesInputPreservesChatImageURL(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)
	result := normalizeResponsesInput(body, "/v1/responses")

	if gjson.GetBytes(result, "messages").Exists() {
		t.Fatalf("messages should be removed after conversion: %s", result)
	}
	content := gjson.GetBytes(result, "input.0.content")
	if got := content.Get("0.type").String(); got != "input_text" {
		t.Fatalf("first content type = %q, want input_text", got)
	}
	if got := content.Get("1.type").String(); got != "input_image" {
		t.Fatalf("second content type = %q, want input_image", got)
	}
	if got := content.Get("1.image_url").String(); got != "data:image/png;base64,AAA" {
		t.Fatalf("image_url = %q", got)
	}
}

func TestNormalizeResponsesInputMovesSystemInputToInstructions(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","instructions":"base","input":[{"role":"system","content":[{"type":"input_text","text":"rule one"},{"type":"input_text","text":"rule two"}]},{"type":"metadata","role":"system","content":"rule three"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	result := normalizeResponsesInput(body, "/v1/responses")

	if got := gjson.GetBytes(result, "instructions").String(); got != "rule one\n\nrule two\n\nrule three\n\nbase" {
		t.Fatalf("instructions = %q; body=%s", got, result)
	}
	if got := gjson.GetBytes(result, "input.#").Int(); got != 1 {
		t.Fatalf("input length = %d, want 1; body=%s", got, result)
	}
	if got := gjson.GetBytes(result, "input.0.role").String(); got != "user" {
		t.Fatalf("remaining role = %q, want user; body=%s", got, result)
	}
	if got := gjson.GetBytes(result, "input.0.content.0.text").String(); got != "hi" {
		t.Fatalf("remaining user text = %q, want hi; body=%s", got, result)
	}
}

func TestNormalizeResponsesInputRepairsInvalidItemIDs(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","id":"msg_valid-1","role":"user","content":"one"},{"type":"message","id":"msg_019f8361:14f6","role":"user","content":"two"},{"type":"message","id":" msg_trimmed_1 ","role":"user","content":"three"},{"type":"function_call","id":"fc.invalid","call_id":"call_valid-1","name":"read","arguments":"{}"},{"type":"message","id":123,"role":"user","content":"four"}]}`)
	result := normalizeResponsesInput(body, "/v1/responses")

	if got := gjson.GetBytes(result, "input.0.id").String(); got != "msg_valid-1" {
		t.Fatalf("valid id = %q, want msg_valid-1; body=%s", got, result)
	}
	if gjson.GetBytes(result, "input.1.id").Exists() {
		t.Fatalf("invalid message id was not removed: %s", result)
	}
	if got := gjson.GetBytes(result, "input.2.id").String(); got != "msg_trimmed_1" {
		t.Fatalf("trimmed id = %q, want msg_trimmed_1; body=%s", got, result)
	}
	if gjson.GetBytes(result, "input.3.id").Exists() {
		t.Fatalf("invalid function item id was not removed: %s", result)
	}
	if got := gjson.GetBytes(result, "input.3.call_id").String(); got != "call_valid-1" {
		t.Fatalf("call_id = %q, want call_valid-1; body=%s", got, result)
	}
	if gjson.GetBytes(result, "input.4.id").Exists() {
		t.Fatalf("non-string item id was not removed: %s", result)
	}
}

func TestNormalizeResponsesInputRepairsInvalidBareItemID(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":{"type":"message","id":"msg.invalid","role":"user","content":"hi"}}`)
	result := normalizeResponsesInput(body, "/v1/responses")

	if gjson.GetBytes(result, "input.id").Exists() {
		t.Fatalf("invalid bare input id was not removed: %s", result)
	}
}

func TestNormalizeResponsesInputMessageIDPrefixes(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		wantID string
	}{
		{name: "valid msg prefix", id: "msg_019f84000000000000000001", wantID: "msg_019f84000000000000000001"},
		{name: "item prefix", id: "item_019f84000000000000000001"},
		{name: "other prefix", id: "foo_019f84000000000000000001"},
		{name: "uppercase prefix", id: "MSG_019f84000000000000000001"},
		{name: "missing underscore", id: "msg019f84000000000000000001"},
		{name: "dash separator", id: "msg-019f84000000000000000001"},
		{name: "empty suffix", id: "msg_"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(
				`{"model":"gpt-5.5","input":[{"type":"message","id":%q,"role":"user","content":"hi"}]}`,
				test.id,
			))
			result := normalizeResponsesInput(body, "/v1/responses")
			id := gjson.GetBytes(result, "input.0.id")
			if test.wantID == "" {
				if id.Exists() {
					t.Fatalf("invalid message id was not removed: %s", result)
				}
				return
			}
			if got := id.String(); got != test.wantID {
				t.Fatalf("message id = %q, want %q; body=%s", got, test.wantID, result)
			}
		})
	}
}

func TestNormalizeResponsesInputFunctionCallIDPrefixes(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		wantID string
	}{
		{name: "valid fc prefix", id: "fc_5y00Mm1j85mIlBCrLUwLMXT7", wantID: "fc_5y00Mm1j85mIlBCrLUwLMXT7"},
		{name: "call id used as item id", id: "call_5y00Mm1j85mIlBCrLUwLMXT7"},
		{name: "other prefix", id: "item_5y00Mm1j85mIlBCrLUwLMXT7"},
		{name: "uppercase prefix", id: "FC_5y00Mm1j85mIlBCrLUwLMXT7"},
		{name: "missing underscore", id: "fc5y00Mm1j85mIlBCrLUwLMXT7"},
		{name: "dash separator", id: "fc-5y00Mm1j85mIlBCrLUwLMXT7"},
		{name: "empty suffix", id: "fc_"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(
				`{"model":"gpt-5.5","input":[{"type":"function_call","id":%q,"call_id":"call_5y00Mm1j85mIlBCrLUwLMXT7","name":"lookup","arguments":"{}"}]}`,
				test.id,
			))
			result := normalizeResponsesInput(body, "/v1/responses")
			id := gjson.GetBytes(result, "input.0.id")
			if test.wantID == "" {
				if id.Exists() {
					t.Fatalf("invalid function_call id was not removed: %s", result)
				}
			} else if got := id.String(); got != test.wantID {
				t.Fatalf("function_call id = %q, want %q; body=%s", got, test.wantID, result)
			}
			if got := gjson.GetBytes(result, "input.0.call_id").String(); got != "call_5y00Mm1j85mIlBCrLUwLMXT7" {
				t.Fatalf("call_id = %q, want preserved; body=%s", got, result)
			}
		})
	}
}

func TestNormalizeResponsesInputCustomToolCallIDPrefixes(t *testing.T) {
	const callID = "call_919b3a9db8d3f2360fa1cfcd"
	tests := []struct {
		name   string
		id     string
		wantID string
	}{
		{name: "valid ctc prefix", id: "ctc_919b3a9db8d3f2360fa1cfcd", wantID: "ctc_919b3a9db8d3f2360fa1cfcd"},
		{name: "generic item prefix", id: "item_919b3a9db8d3f2360fa1cfcd"},
		{name: "function call prefix", id: "fc_919b3a9db8d3f2360fa1cfcd"},
		{name: "call id used as item id", id: callID},
		{name: "uppercase prefix", id: "CTC_919b3a9db8d3f2360fa1cfcd"},
		{name: "missing underscore", id: "ctc919b3a9db8d3f2360fa1cfcd"},
		{name: "empty suffix", id: "ctc_"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(
				`{"model":"gpt-5.5","input":[{"type":"custom_tool_call","id":%q,"call_id":%q,"name":"exec","input":"pwd"}]}`,
				test.id,
				callID,
			))
			result := normalizeResponsesInput(body, "/v1/responses")
			id := gjson.GetBytes(result, "input.0.id")
			if test.wantID == "" {
				if id.Exists() {
					t.Fatalf("invalid custom_tool_call id was not removed: %s", result)
				}
			} else if got := id.String(); got != test.wantID {
				t.Fatalf("custom_tool_call id = %q, want %q; body=%s", got, test.wantID, result)
			}
			if got := gjson.GetBytes(result, "input.0.call_id").String(); got != callID {
				t.Fatalf("call_id = %q, want %q preserved; body=%s", got, callID, result)
			}
		})
	}
}

func TestNormalizeResponsesInputPreservesExistingFunctionCallID(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"function_call","id":"call_wrong","call_id":"call_existing","name":"lookup","arguments":"{}"}]}`)
	result := normalizeResponsesInput(body, "/v1/responses")

	if gjson.GetBytes(result, "input.0.id").Exists() {
		t.Fatalf("invalid function_call id was not removed: %s", result)
	}
	if got := gjson.GetBytes(result, "input.0.call_id").String(); got != "call_existing" {
		t.Fatalf("call_id = %q, want preserved existing value; body=%s", got, result)
	}
}

func TestPreprocessRequestBodyRepairsFunctionCallIDPrefix(t *testing.T) {
	const callID = "call_5y00Mm1j85mIlBCrLUwLMXT7"
	body := []byte(`{"model":"gpt-5.5","input":[` +
		`{"type":"message","role":"user","content":"use the tool result"},` +
		`{"type":"function_call","id":"` + callID + `","call_id":"` + callID + `","name":"lookup","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"` + callID + `","output":"OK"}` +
		`]}`)
	result := preprocessRequestBody(body, "gpt-5.5", "/v1/responses")

	if gjson.GetBytes(result, "input.1.id").Exists() {
		t.Fatalf("invalid input[1].id was not removed: %s", result)
	}
	if got := gjson.GetBytes(result, "input.1.call_id").String(); got != callID {
		t.Fatalf("function_call call_id = %q, want %q; body=%s", got, callID, result)
	}
	if got := gjson.GetBytes(result, "input.2.call_id").String(); got != callID {
		t.Fatalf("function_call_output call_id = %q, want %q; body=%s", got, callID, result)
	}
}

func TestPreprocessRequestBodyRepairsCustomToolCallIDPrefix(t *testing.T) {
	const callID = "call_919b3a9db8d3f2360fa1cfcd"
	body := []byte(`{"model":"gpt-5.5","input":[` +
		`{"type":"custom_tool_call","id":"item_919b3a9db8d3f2360fa1cfcd","call_id":"` + callID + `","name":"exec","input":"pwd"},` +
		`{"type":"custom_tool_call_output","call_id":"` + callID + `","output":"ok"}` +
		`]}`)
	result := preprocessRequestBody(body, "gpt-5.5", "/v1/responses")

	if gjson.GetBytes(result, "input.0.id").Exists() {
		t.Fatalf("invalid custom_tool_call id was not removed: %s", result)
	}
	if got := gjson.GetBytes(result, "input.0.call_id").String(); got != callID {
		t.Fatalf("custom_tool_call call_id = %q, want %q; body=%s", got, callID, result)
	}
	if got := gjson.GetBytes(result, "input.1.call_id").String(); got != callID {
		t.Fatalf("custom_tool_call_output call_id = %q, want %q; body=%s", got, callID, result)
	}
}

func TestNormalizeResponsesInputPreservesEncryptedReasoningAndStripsRSID(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"sealed","content":[{"type":"reasoning_text","text":"keep"}],"extra":"field"},{"type":"reasoning","id":"other_1","encrypted_content":"sealed2","summary":[{"type":"summary_text","text":"visible"}]}]}`)
	result := normalizeResponsesInput(body, "/v1/responses")

	if gjson.GetBytes(result, "input.0.id").Exists() {
		t.Fatalf("rs_* reasoning id was not removed: %s", result)
	}
	if got := gjson.GetBytes(result, "input.0.encrypted_content").String(); got != "sealed" {
		t.Fatalf("encrypted_content = %q, want sealed; body=%s", got, result)
	}
	if got := gjson.GetBytes(result, "input.0.content.0.text").String(); got != "keep" {
		t.Fatalf("content text = %q, want keep; body=%s", got, result)
	}
	if got := gjson.GetBytes(result, "input.0.extra").String(); got != "field" {
		t.Fatalf("extra field = %q, want field; body=%s", got, result)
	}
	if !gjson.GetBytes(result, "input.0.summary").IsArray() {
		t.Fatalf("summary was not backfilled: %s", result)
	}
	if got := gjson.GetBytes(result, "input.1.id").String(); got != "other_1" {
		t.Fatalf("non-rs id = %q, want other_1; body=%s", got, result)
	}
	if got := gjson.GetBytes(result, "input.1.summary.0.text").String(); got != "visible" {
		t.Fatalf("existing summary was not preserved; body=%s", result)
	}
}

func TestNormalizeResponsesInputBackfillsBareReasoningSummary(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":{"type":"reasoning","id":"rs_bare"}}`)
	result := normalizeResponsesInput(body, "/v1/responses")

	if gjson.GetBytes(result, "input.id").Exists() {
		t.Fatalf("rs_* reasoning id was not removed: %s", result)
	}
	if !gjson.GetBytes(result, "input.summary").IsArray() {
		t.Fatalf("summary was not backfilled: %s", result)
	}
}

func TestPreprocessRequestBody_ForcesResponsesStoreFalse(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{
			name: "responses input",
			body: []byte(`{"model":"gpt-5.4","input":"hi","store":true}`),
		},
		{
			name: "responses messages",
			body: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"store":true}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := preprocessRequestBody(tc.body, "gpt-5.4", "/v1/responses")
			if store := gjson.GetBytes(got, "store"); !store.Exists() || store.Bool() {
				t.Fatalf("store = %v, want false; body=%s", store.Value(), got)
			}
		})
	}
}

func TestPreprocessRequestBodyRepairsInvalidResponsesInputItemID(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","id":"msg_019f8361/14f6","role":"user","content":"hi"}]}`)
	result := preprocessRequestBody(body, "gpt-5.6-sol", "/v1/responses")

	if gjson.GetBytes(result, "input.0.id").Exists() {
		t.Fatalf("invalid input item id reached the upstream body: %s", result)
	}
	if got := gjson.GetBytes(result, "input.0.content").String(); got != "hi" {
		t.Fatalf("message content = %q, want hi; body=%s", got, result)
	}
}

func TestPreprocessRequestBodyPreservesPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_old","input":[]}`)
	got := preprocessRequestBody(body, "gpt-5.4", "/v1/responses")
	if previous := gjson.GetBytes(got, "previous_response_id").String(); previous != "resp_old" {
		t.Fatalf("previous_response_id = %q, want resp_old; body=%s", previous, got)
	}
}

func TestResolveEffectiveModelFallsBackToDefaultModel(t *testing.T) {
	t.Parallel()

	for _, requested := range []string{"", "None", "gpt-unknown", "gpt-5.4"} {
		for _, existing := range []any{"", "None", "gpt-unknown", "gpt-5.4", nil} {
			if got := resolveEffectiveModel(requested, existing); got != "gpt-5.6-luna" {
				t.Fatalf("resolveEffectiveModel(%q, %#v) = %q, want gpt-5.6-luna", requested, existing, got)
			}
		}
	}
}

func TestFirstNonEmptyTier_RequestFastFallsBackToUpstreamPriority(t *testing.T) {
	if got := firstNonEmptyTier("fast", "priority"); got != "priority" {
		t.Fatalf("firstNonEmptyTier(fast, priority) = %q, want %q", got, "priority")
	}
}
