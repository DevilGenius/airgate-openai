package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestTextSafetyRequestCacheDefaults(t *testing.T) {
	if textSafetyRequestCacheTTL != 24*time.Hour {
		t.Fatalf("default TTL = %s, want 24h", textSafetyRequestCacheTTL)
	}
	if textSafetyCacheRetryAfter != 10*time.Minute {
		t.Fatalf("cache retry after = %s, want 10m", textSafetyCacheRetryAfter)
	}
	if textSafetyRequestCacheMaxEntries != 8192 {
		t.Fatalf("default max entries = %d, want 8192", textSafetyRequestCacheMaxEntries)
	}
}

func TestTextRequestHashSeparatesRequests(t *testing.T) {
	req := &sdk.ForwardRequest{
		Body:    []byte(`{"model":"gpt-5.4","input":"same prompt"}`),
		Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Model:   "gpt-5.4",
	}
	first, ok := textRequestHash(req, http.MethodPost, "/v1/responses")
	if !ok {
		t.Fatal("responses request should produce a hash")
	}
	copyReq := *req
	second, ok := textRequestHash(&copyReq, http.MethodPost, "/v1/responses")
	if !ok || first != second {
		t.Fatal("identical text requests should produce the same hash")
	}
	changedReq := *req
	changedReq.Body = []byte(`{"model":"gpt-5.4","input":"different prompt"}`)
	changed, _ := textRequestHash(&changedReq, http.MethodPost, "/v1/responses")
	if first == changed {
		t.Fatal("different text requests must not share a hash")
	}
	if _, ok := textRequestHash(req, http.MethodPost, "/v1/images/generations"); ok {
		t.Fatal("image request should not produce a text safety hash")
	}
	if _, ok := textRequestHash(req, http.MethodPost, "/v1/responses/compact"); ok {
		t.Fatal("compact request should not produce a text safety hash")
	}
}

func TestTextPromptHashIgnoresTransportAndEncryptedContent(t *testing.T) {
	firstCiphertext := validGPTReasoningEncryptedContentForTestMarker(0x11)
	secondCiphertext := validGPTReasoningEncryptedContentForTestMarker(0x22)
	first := &sdk.ForwardRequest{
		Body: []byte(`{"model":"gpt-5.4","stream":true,"input":[` +
			`{"id":"rs_first","type":"reasoning","encrypted_content":"` + firstCiphertext + `","summary":[]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"same prompt"}]}` +
			`]}`),
		Model:  "gpt-5.4",
		Stream: true,
	}
	second := &sdk.ForwardRequest{
		Body: []byte(`{"model":"gpt-5.4","stream":false,"input":[` +
			`{"id":"rs_second","type":"reasoning","encrypted_content":"` + secondCiphertext + `","summary":[]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"same prompt"}]}` +
			`]}`),
		Model: "gpt-5.4",
	}
	firstHash, firstOK := textPromptHash(first, http.MethodPost, "/v1/responses")
	secondHash, secondOK := textPromptHash(second, http.MethodPost, "/v1/responses")
	if !firstOK || !secondOK || firstHash != secondHash {
		t.Fatalf("prompt hashes = %d/%d ok=%v/%v, want equal", firstHash, secondHash, firstOK, secondOK)
	}
	second.Body = []byte(`{"model":"gpt-5.4","input":"different prompt"}`)
	differentHash, ok := textPromptHash(second, http.MethodPost, "/v1/responses")
	if !ok || differentHash == firstHash {
		t.Fatal("different prompt must produce a different prompt hash")
	}
}

func TestTextSafetyRequestCacheReturns429Immediately(t *testing.T) {
	gateway := &OpenAIGateway{}
	req := &sdk.ForwardRequest{
		Body:    []byte(`{"model":"gpt-5.4","input":"blocked prompt"}`),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Model:   "gpt-5.4",
	}
	ctx, outcome := gateway.checkTextSafetyRequest(context.Background(), req, http.MethodPost, "/v1/responses")
	if outcome != nil {
		t.Fatal("uncached request should continue to upstream")
	}
	gateway.cacheTextSafetyRejection(ctx)
	for attempt := 1; attempt <= 2; attempt++ {
		start := time.Now()
		_, outcome = gateway.checkTextSafetyRequest(context.Background(), req, http.MethodPost, "/v1/responses")
		if elapsed := time.Since(start); elapsed >= time.Second {
			t.Fatalf("cached request %d returned after %s, want immediate 429", attempt, elapsed)
		}
		if outcome == nil {
			t.Fatalf("cached request %d should be rejected locally", attempt)
		}
		if outcome.Kind != sdk.OutcomeClientError || outcome.Upstream.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("outcome = %#v, want client error 429", outcome)
		}
		if got := gjson.GetBytes(outcome.Upstream.Body, "error.code").String(); got != textSafetyRateLimitCode {
			t.Fatalf("error.code = %q, want %q", got, textSafetyRateLimitCode)
		}
		if got := gjson.GetBytes(outcome.Upstream.Body, "error.message").String(); got != textSafetyRateLimitMessage {
			t.Fatalf("error.message = %q, want %q", got, textSafetyRateLimitMessage)
		}
		if got := outcome.Upstream.Headers.Get("Retry-After"); got != "600" {
			t.Fatalf("Retry-After = %q, want 600", got)
		}
		if outcome.RetryAfter != textSafetyCacheRetryAfter {
			t.Fatalf("RetryAfter = %s, want %s", outcome.RetryAfter, textSafetyCacheRetryAfter)
		}
		if outcome.SafetyRejected {
			t.Fatal("local cache hit must not be reported as a new upstream safety rejection")
		}
	}
}

func TestTextSafetyCacheHitOutcomeUsesAnthropicEnvelope(t *testing.T) {
	outcome := textSafetyCacheHitOutcome(true)
	if outcome.Upstream.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", outcome.Upstream.StatusCode)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "type").String(); got != "error" {
		t.Fatalf("type = %q, want error", got)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "error.code").String(); got != textSafetyRateLimitCode {
		t.Fatalf("error.code = %q, want %q", got, textSafetyRateLimitCode)
	}
}

func TestCybersecurityRiskClassificationSetsSafetyRejected(t *testing.T) {
	failure := classifyResponsesError("invalid_request", "cyber_policy", cybersecurityRiskMessage)
	if failure == nil || failure.Kind != responsesFailureKindClient || failure.Code != cybersecurityRiskErrorCode {
		t.Fatalf("failure = %#v, want client safety rejection", failure)
	}
	if !failure.isSafetyRejected() {
		t.Fatal("explicit cyber_policy failure should be classified as SafetyRejected")
	}
	if got := classifyHTTPFailure(http.StatusInternalServerError, cybersecurityRiskMessage); got != sdk.OutcomeUpstreamTransient {
		t.Fatalf("message-only HTTP classification = %v, want upstream transient", got)
	}
	rejected := failureOutcome(
		http.StatusInternalServerError,
		[]byte(`{"error":{"type":"invalid_request","code":"cyber_policy","message":"`+cybersecurityRiskMessage+`"}}`),
		nil,
		"generic upstream failure",
		0,
	)
	if !rejected.SafetyRejected {
		t.Fatal("explicit cyber_policy error should set the safety flag")
	}
	messageOnly := failureOutcome(
		http.StatusBadRequest,
		[]byte(cybersecurityRiskMessage),
		nil,
		cybersecurityRiskMessage,
		0,
	)
	if messageOnly.SafetyRejected {
		t.Fatal("cybersecurity wording without an explicit error code must not set the safety flag")
	}
	ordinary := failureOutcome(
		http.StatusInternalServerError,
		[]byte(`{"error":{"message":"temporary upstream failure"}}`),
		nil,
		"temporary upstream failure",
		0,
	)
	if ordinary.SafetyRejected {
		t.Fatal("ordinary upstream failure must not set the safety flag")
	}
}

func TestInvalidPromptClassificationSetsSafetyRejected(t *testing.T) {
	failure := classifyResponsesError("invalid_request", promptUsagePolicyErrorCode, "prompt rejected")
	if failure == nil || failure.Kind != responsesFailureKindClient || failure.Code != promptUsagePolicyErrorCode {
		t.Fatalf("failure = %#v, want invalid_prompt safety rejection", failure)
	}
	if !failure.isSafetyRejected() {
		t.Fatal("explicit invalid_prompt failure should be classified as SafetyRejected")
	}

	rejected := failureOutcome(
		http.StatusBadRequest,
		[]byte(`{"error":{"type":"invalid_request","code":"invalid_prompt","message":"prompt rejected"}}`),
		nil,
		"prompt rejected",
		0,
	)
	if !rejected.SafetyRejected {
		t.Fatal("explicit invalid_prompt error should set the safety flag")
	}

	invalidEncryptedContent := failureOutcome(
		http.StatusBadRequest,
		[]byte(`{"error":{"type":"invalid_request","code":"invalid_encrypted_content","message":"bad ciphertext"}}`),
		nil,
		"bad ciphertext",
		0,
	)
	if invalidEncryptedContent.SafetyRejected {
		t.Fatal("invalid_encrypted_content is a continuation error, not a safety rejection")
	}
}

func TestParseExplicitUpstreamErrorRequiresStructuredCode(t *testing.T) {
	rejection, ok := parseExplicitUpstreamError([]byte(`{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"blocked"}}}`))
	if !ok || rejection.UpstreamCode != "cyber_policy" || rejection.Code != cybersecurityRiskErrorCode {
		t.Fatalf("explicit rejection = %+v ok=%v", rejection, ok)
	}
	if _, ok := parseExplicitUpstreamError([]byte(`{"error":{"message":"cyber_policy blocked"}}`)); ok {
		t.Fatal("message-only error must not be treated as an explicit rejection")
	}
	wrapped := []byte(`{"error":{"message":"{\"error\":{\"code\":\"invalid_encrypted_content\",\"message\":\"bad ciphertext\"}}"}}`)
	rejection, ok = parseExplicitUpstreamError(wrapped)
	if !ok || rejection.Code != "invalid_encrypted_content" {
		t.Fatalf("wrapped explicit rejection = %+v ok=%v", rejection, ok)
	}
}

func TestHandleNonStreamResponseDoesNotScanSuccessfulAssistantText(t *testing.T) {
	body := openAIErrorJSON("invalid_request_error", cybersecurityRiskErrorCode, cybersecurityRiskMessage)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
	outcome, err := handleNonStreamResponse(resp, nil, time.Now(), "")
	if err != nil {
		t.Fatalf("handleNonStreamResponse error = %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success", outcome.Kind)
	}
	if outcome.SafetyRejected {
		t.Fatal("successful assistant text must not be treated as a structured upstream rejection")
	}
}

func TestHandleStreamResponseDoesNotScanSuccessfulOutputDeltas(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"This content was flagged for possible cyber"}`,
		`data: {"type":"response.output_text.delta","delta":"security risk. If this seems wrong, try rephrasing your request. "}`,
		`data: {"type":"response.output_text.delta","delta":"To get authorized for security work, join the Trusted Access for Cyber program: "}`,
		`data: {"type":"response.output_text.delta","delta":"https://chatgpt.com/cyber"}`,
		`data: {"type":"response.completed","response":{"id":"resp_safety","model":"gpt-5.4","usage":{"input_tokens":1,"output_tokens":1}}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	outcome, err := handleStreamResponse(resp, httptest.NewRecorder(), time.Now(), "")
	if err != nil {
		t.Fatalf("handleStreamResponse error = %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success", outcome.Kind)
	}
	if outcome.SafetyRejected {
		t.Fatal("successful output deltas must not be treated as a structured upstream rejection")
	}
}

func TestHandleStreamResponseMarksStructuredCybersecurityFailure(t *testing.T) {
	sse := `data: {"type":"response.failed","response":{"error":{"type":"invalid_request","code":"cyber_policy","message":"` + cybersecurityRiskMessage + `"}}}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	outcome, err := handleStreamResponse(resp, httptest.NewRecorder(), time.Now(), "")
	if err != nil {
		t.Fatalf("handleStreamResponse error = %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError {
		t.Fatalf("outcome kind = %v, want client error", outcome.Kind)
	}
	if !outcome.SafetyRejected {
		t.Fatal("structured response.failed event must set SafetyRejected")
	}
}

func TestParseResponsesFailureEventMarksTopLevelCybersecurityError(t *testing.T) {
	raw := []byte(`{"type":"error","error":{"type":"invalid_request","code":"cyber_policy","message":"` + cybersecurityRiskMessage + `"}}`)
	err := parseResponsesFailureEvent("error", raw)
	var failure *responsesFailureError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want responsesFailureError", err)
	}
	if !failure.isSafetyRejected() {
		t.Fatalf("failure = %#v, want safety rejection", failure)
	}
}
