package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestClassifyResponsesFailureContextWindow(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("unexpected kind %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status %d", failure.StatusCode)
	}
	if failure.AnthropicErrorType != "invalid_request_error" {
		t.Fatalf("unexpected anthropic error type %q", failure.AnthropicErrorType)
	}
	if failure.Code != "context_too_large" {
		t.Fatalf("unexpected code %q", failure.Code)
	}
	if failure.Message != contextTooLargeMessage {
		t.Fatalf("unexpected message %q", failure.Message)
	}
}

func TestClassifyWSErrorEventRequestTooLarge(t *testing.T) {
	raw := []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"request_too_large","message":"HTTP 413 request entity too large"}}`)
	failure := classifyWSErrorEvent(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("unexpected kind %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", failure.StatusCode)
	}
	if failure.Code != "context_too_large" {
		t.Fatalf("code = %q, want context_too_large", failure.Code)
	}
	if failure.Message != contextTooLargeMessage {
		t.Fatalf("message = %q, want context message", failure.Message)
	}
}

func TestClassifyResponsesFailureSafetyRejected(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"Your request was rejected by the safety system. If you believe this is an error, contact us at help.openai.com and include the request ID 916c6516-5f37-9121-b05a-a604888c0055."}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("unexpected kind %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status %d", failure.StatusCode)
	}
	if failure.Code != "safety_rejected" {
		t.Fatalf("unexpected code %q", failure.Code)
	}
}

// TestIsSafetyRejectionTextNearMisses 守住关键词收紧后的边界：
// 提示性文案、把 policy 当名词解释的 400 不应被误判成"安全拒绝"。
func TestIsSafetyRejectionTextNearMisses(t *testing.T) {
	negatives := []string{
		"please ensure your prompt follows our safety policy guidelines",
		"see the safety policy for details",
		"input violates the company policy",
		"this field requires the safety token",
		"",
	}
	for _, msg := range negatives {
		if isSafetyRejectionText(msg) {
			t.Fatalf("did not expect safety match for %q", msg)
		}
	}

	positives := []string{
		"Your request was rejected by the safety system",
		"content_policy_violation",
		"blocked by policy",
		"prompt was blocked by our safety filter",
		"moderation_blocked",
	}
	for _, msg := range positives {
		if !isSafetyRejectionText(msg) {
			t.Fatalf("expected safety match for %q", msg)
		}
	}
}

func TestClassifyResponsesFailureContinuationAnchor(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found"}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindContinuationAnchor {
		t.Fatalf("unexpected kind %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status %d", failure.StatusCode)
	}
	if failure.Code != "previous_response_not_found" {
		t.Fatalf("unexpected code %q", failure.Code)
	}
	if !failure.isContinuationAnchorError() {
		t.Fatalf("expected continuation anchor error")
	}
}

func TestClassifyWSErrorEventFunctionCallOutputWithoutCall(t *testing.T) {
	raw := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"No tool call found for function call output with call_id call_1."}}`)

	failure := classifyWSErrorEvent(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindContinuationAnchor {
		t.Fatalf("kind = %q, want continuation_anchor", failure.Kind)
	}
	if failure.Code != "function_call_output_without_call" {
		t.Fatalf("code = %q, want function_call_output_without_call", failure.Code)
	}
}

func TestClassifyResponsesFailureEncryptedContentVerifyFailed(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","message":"encrypted content verify failed"}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindContinuationAnchor {
		t.Fatalf("unexpected kind %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status %d", failure.StatusCode)
	}
	if failure.Code != "invalid_encrypted_content" {
		t.Fatalf("unexpected code %q", failure.Code)
	}
	if kind := failure.outcomeKind(); kind != sdk.OutcomeClientError {
		t.Fatalf("expected OutcomeClientError, got %v", kind)
	}
}

func TestClassifyResponsesFailureInvalidEncryptedContentVariants(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified."}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindContinuationAnchor {
		t.Fatalf("unexpected kind %q", failure.Kind)
	}
	if failure.Code != "invalid_encrypted_content" {
		t.Fatalf("unexpected code %q", failure.Code)
	}

	wsRaw := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"The encrypted content could not be verified."}}`)
	wsFailure := classifyWSErrorEvent(wsRaw)
	if wsFailure == nil {
		t.Fatalf("expected websocket failure")
	}
	if wsFailure.Kind != responsesFailureKindContinuationAnchor {
		t.Fatalf("unexpected websocket kind %q", wsFailure.Kind)
	}
	if wsFailure.Code != "invalid_encrypted_content" {
		t.Fatalf("unexpected websocket code %q", wsFailure.Code)
	}
}

func TestOutcomeIsPreviousResponseNotFound(t *testing.T) {
	outcome := sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Body:       []byte(`{"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found"}}`),
		},
	}
	if !outcomeIsPreviousResponseNotFound(outcome) {
		t.Fatalf("expected previous_response_not_found outcome")
	}

	outcome.Upstream.Body = []byte(`{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified."}}`)
	if outcomeIsPreviousResponseNotFound(outcome) {
		t.Fatalf("invalid_encrypted_content must not trigger previous_response_not_found recovery")
	}
}

func TestClassifyHTTPFailureTreatsUsageLimit403AsRateLimited(t *testing.T) {
	got := classifyHTTPFailure(403, "The usage limit has been reached. Please try again later.")
	if got != sdk.OutcomeAccountRateLimited {
		t.Fatalf("expected AccountRateLimited, got %v", got)
	}
}

func TestClassifyHTTPFailureTreatsUsageLimit400AsRateLimited(t *testing.T) {
	got := classifyHTTPFailure(400, "The usage limit has been reached. Please try again later.")
	if got != sdk.OutcomeAccountRateLimited {
		t.Fatalf("expected AccountRateLimited, got %v", got)
	}
}

func TestClassifyHTTPFailureTreatsOverloaded429AsFamilyTransient(t *testing.T) {
	got := classifyHTTPFailure(429, "Our servers are currently overloaded. Please try again later.")
	if got != sdk.OutcomeFamilyTransient {
		t.Fatalf("expected FamilyTransient, got %v", got)
	}
}

func TestClassifyHTTPFailureKeepsDisabled403AsAccountDead(t *testing.T) {
	got := classifyHTTPFailure(403, "Organization disabled due to policy violation")
	if got != sdk.OutcomeAccountDead {
		t.Fatalf("expected AccountDead, got %v", got)
	}
}

func TestClassifyHTTPFailureTreatsPlain403AsAccountUnavailable(t *testing.T) {
	got := classifyHTTPFailure(403, "访问被拒绝，账号可能已被禁用或无权限 (HTTP 403)")
	if got != sdk.OutcomeAccountUnavailable {
		t.Fatalf("expected AccountUnavailable, got %v", got)
	}
}

func TestClassifyHTTPFailureTreatsDisabled400AsAccountDead(t *testing.T) {
	got := classifyHTTPFailure(400, "Organization disabled due to policy violation")
	if got != sdk.OutcomeAccountDead {
		t.Fatalf("expected AccountDead, got %v", got)
	}
}

func TestClassifyAnthropicBodyTreatsUsageLimit403AsRateLimited(t *testing.T) {
	body := []byte(`{"error":{"message":"The usage limit has been reached. Try again later."}}`)
	got := classifyAnthropicBody(403, body)
	if got != sdk.OutcomeAccountRateLimited {
		t.Fatalf("expected AccountRateLimited, got %v", got)
	}
}

func TestClassifyWSErrorEventUsageLimitReached(t *testing.T) {
	// ChatGPT OAuth 触发 usage limit 时走 WS error 事件，带 resets_in_seconds。
	raw := []byte(`{"type":"error","error":{"type":"usage_limit_reached","code":"rate_limit_exceeded","message":"The usage limit has been reached","resets_in_seconds":3600}}`)
	failure := classifyWSErrorEvent(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindRateLimited {
		t.Fatalf("expected rate_limited kind, got %q", failure.Kind)
	}
	if kind := failure.outcomeKind(); kind != sdk.OutcomeAccountRateLimited {
		t.Fatalf("expected OutcomeAccountRateLimited, got %v", kind)
	}
	if failure.RetryAfter < 59*time.Minute || failure.RetryAfter > 61*time.Minute {
		t.Fatalf("expected RetryAfter~=1h from resets_in_seconds, got %s", failure.RetryAfter)
	}
}

func TestClassifyResponsesFailureOverloadedIsFamilyTransient(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"server_error","code":"server_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindFamilyTransient {
		t.Fatalf("expected family_transient kind, got %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", failure.StatusCode)
	}
	if kind := failure.outcomeKind(); kind != sdk.OutcomeFamilyTransient {
		t.Fatalf("expected OutcomeFamilyTransient, got %v", kind)
	}
}

func TestClassifyWSErrorEventOpenAICompatSSEError(t *testing.T) {
	raw := []byte(`{"error":{"message":"An error occurred while processing your request. Please include the request ID 349f8894 in your message.","type":"server_error","code":"upstream_error"}}`)
	failure := classifyWSErrorEvent(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindServer {
		t.Fatalf("expected server kind, got %q", failure.Kind)
	}
	if failure.Message != "An error occurred while processing your request. Please include the request ID 349f8894 in your message." {
		t.Fatalf("unexpected message %q", failure.Message)
	}
}

func TestClassifyGenericSSEErrorEventTopLevelModelNotFound(t *testing.T) {
	raw := []byte(`{"message":"The model gpt-5.3-codex-spark does not exist.","type":"invalid_request_error","code":"model_not_found"}`)
	failure := classifyGenericSSEErrorEvent(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("expected client kind, got %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400, got %d", failure.StatusCode)
	}
	if kind := failure.outcomeKind(); kind != sdk.OutcomeClientError {
		t.Fatalf("expected OutcomeClientError, got %v", kind)
	}
	if scope := failure.failoverScopeForKind(failure.outcomeKind()); scope != sdk.FailoverScopeDispatchCandidate {
		t.Fatalf("expected dispatch candidate failover scope, got %q", scope)
	}
}

func TestFailureOutcomeModelNotFoundRequestsDispatchCandidateFailover(t *testing.T) {
	outcome := failureOutcome(
		http.StatusBadRequest,
		[]byte(`{"error":{"message":"The model gpt-5.3-codex-spark does not exist.","code":"model_not_found"}}`),
		http.Header{"Content-Type": []string{"application/json"}},
		"The model gpt-5.3-codex-spark does not exist.",
		0,
	)

	if outcome.Kind != sdk.OutcomeClientError {
		t.Fatalf("Kind = %v, want OutcomeClientError", outcome.Kind)
	}
	if outcome.FailoverScope != sdk.FailoverScopeDispatchCandidate {
		t.Fatalf("FailoverScope = %q, want dispatch candidate", outcome.FailoverScope)
	}
}

func TestHandleStreamResponseSanitizesFirstSSEError(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("data: {\"error\":{\"message\":\"upstream secret request ID 349f8894\",\"type\":\"server_error\",\"code\":\"upstream_error\"}}\n\n")),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("expected OutcomeUpstreamTransient, got %v", outcome.Kind)
	}
	body := w.Body.String()
	if strings.Contains(body, "upstream secret") || strings.Contains(body, "349f8894") {
		t.Fatalf("response leaked upstream error: %q", body)
	}
}

func TestHandleStreamResponseTreatsCompletedEmptyStreamAsFailure(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err == nil {
		t.Fatalf("expected empty stream error")
	}
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("expected OutcomeUpstreamTransient, got %v", outcome.Kind)
	}
	if !strings.Contains(outcome.Reason, "上游流式响应为空") {
		t.Fatalf("unexpected reason %q", outcome.Reason)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("empty stream should not be forwarded before validation, got %q", w.Body.String())
	}
}

func TestHandleStreamResponseFlushesBufferedPreludeWhenOutputArrives(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("expected OutcomeSuccess, got %v", outcome.Kind)
	}
	got := w.Body.String()
	if !strings.Contains(got, `"role":"assistant"`) || !strings.Contains(got, `"content":"ok"`) || !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("buffered stream was not forwarded completely: %q", got)
	}
}

func TestClassifyResponsesFailureResetsAtAbsolute(t *testing.T) {
	// resets_at 是 Unix 时间戳（绝对时间），RetryAfter 应该反推出大致等于
	// future - now；这里留充分的断言窗口避免时钟抖动。
	future := time.Now().Add(2 * time.Hour).Unix()
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":` + formatInt(future) + `}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil || failure.Kind != responsesFailureKindRateLimited {
		t.Fatalf("expected rate_limited failure, got %+v", failure)
	}
	if failure.RetryAfter < time.Hour+30*time.Minute || failure.RetryAfter > 2*time.Hour+5*time.Minute {
		t.Fatalf("expected RetryAfter~=2h, got %s", failure.RetryAfter)
	}
}

func formatInt(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	buf := make([]byte, 0, 20)
	for v > 0 {
		buf = append([]byte{digits[v%10]}, buf...)
		v /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
