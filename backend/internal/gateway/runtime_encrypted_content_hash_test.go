package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func encryptedContentRetryTestRequest(ciphertext, suffix string) *sdk.ForwardRequest {
	return &sdk.ForwardRequest{
		Body: []byte(`{"model":"gpt-5.4","input":[` +
			`{"id":"rs_retry","type":"reasoning","encrypted_content":"` + ciphertext + `","summary":[]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue` + suffix + `"}]}` +
			`]}`),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Model:   "gpt-5.4",
		Stream:  true,
	}
}

func preprocessEncryptedContentRetryTestRequest(gateway *OpenAIGateway, req *sdk.ForwardRequest) *encryptedContentRetryRequestState {
	state := gateway.newEncryptedContentRetryRequestState()
	req.Body = preprocessRequestBodyWithEncryptedContentState(
		req.Body,
		req.Model,
		"/v1/responses",
		state,
		req.Headers,
	)
	return state
}

func abbreviatedEncryptedContentForTest(raw string) string {
	return raw[:8] + "..." + raw[len(raw)-4:]
}

func TestSafetyRejectionRetryMatchesCiphertextAcrossBodyChanges(t *testing.T) {
	rejected := validGPTReasoningEncryptedContentForTestMarker(0x11)
	fresh := validGPTReasoningEncryptedContentForTestMarker(0x22)
	gateway := &OpenAIGateway{}
	first := encryptedContentRetryTestRequest(rejected, "")

	firstState := preprocessEncryptedContentRetryTestRequest(gateway, first)
	if firstState.sanitized {
		t.Fatal("first request must preserve encrypted_content")
	}
	if got := gjson.GetBytes(first.Body, "input.0.encrypted_content").String(); got != rejected {
		t.Fatalf("first request encrypted_content = %q, want preserved", got)
	}
	if !firstState.CacheViolation(explicitUpstreamError{Code: promptUsagePolicyErrorCode}) {
		t.Fatal("Prompt rejection should cache the ciphertext hash")
	}

	retry := &sdk.ForwardRequest{Body: []byte(`{"model":"gpt-5.4","input":[` +
		`{"id":"rs_rejected","type":"reasoning","encrypted_content":"` + rejected + `","summary":[]},` +
		`{"id":"rs_fresh","type":"reasoning","encrypted_content":"` + fresh + `","summary":[]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue with newly added text"}]}` +
		`]}`)}
	retryState := preprocessEncryptedContentRetryTestRequest(gateway, retry)
	if !retryState.sanitized {
		t.Fatal("retry containing the rejected ciphertext should remove it despite body changes")
	}
	if gjson.GetBytes(retry.Body, "input.0.encrypted_content").Exists() || gjson.GetBytes(retry.Body, "input.0.id").Exists() {
		t.Fatalf("retry retained rejected encrypted reasoning data: %s", retry.Body)
	}
	if got := gjson.GetBytes(retry.Body, "input.1.encrypted_content").String(); got != fresh {
		t.Fatalf("retry removed newly added ciphertext: got %q, want %q", got, fresh)
	}

	requestWithFreshCiphertext := encryptedContentRetryTestRequest(fresh, " with the same surrounding text")
	freshState := preprocessEncryptedContentRetryTestRequest(gateway, requestWithFreshCiphertext)
	if freshState.sanitized {
		t.Fatal("an uncached ciphertext must not be removed")
	}
	if got := gjson.GetBytes(requestWithFreshCiphertext.Body, "input.0.encrypted_content").String(); got != fresh {
		t.Fatalf("fresh encrypted_content = %q, want preserved", got)
	}
}

func TestSafetyRejectionCachesOnlyReasoningCiphertextStrings(t *testing.T) {
	gateway := &OpenAIGateway{}
	for _, body := range [][]byte{
		[]byte(`{"input":[{"type":"reasoning","encrypted_content":null},{"type":"reasoning","encrypted_content":123}]}`),
		[]byte(`{"input":[{"type":"compaction","encrypted_content":"` + validGPTReasoningEncryptedContentForTest() + `"}]}`),
	} {
		req := &sdk.ForwardRequest{Body: body, Model: "gpt-5.4"}
		state := preprocessEncryptedContentRetryTestRequest(gateway, req)
		if state.CacheViolation(explicitUpstreamError{Code: promptUsagePolicyErrorCode}) {
			t.Fatalf("invalid or non-reasoning encrypted content was cached: %s", body)
		}
	}
}

func TestForwardInvalidEncryptedContentReturnsClientError(t *testing.T) {
	const errorObject = `{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content from the previous response could not be verified.","param":"input[0].encrypted_content"}`
	const errorBody = `{"error":` + errorObject + `}`
	for _, tc := range []struct {
		name          string
		eventType     string
		outputStarted bool
	}{
		{name: "HTTP"},
		{name: "SSE response.failed", eventType: "response.failed"},
		{name: "SSE error", eventType: "error"},
		{name: "SSE after output", eventType: "response.failed", outputStarted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ciphertext := validGPTReasoningEncryptedContentForTest()
			var requestCount atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				requestCount.Add(1)
				if got := gjson.GetBytes(body, "input.0.encrypted_content").String(); got != ciphertext {
					t.Errorf("request lost ciphertext: %s", body)
				}
				if got := gjson.GetBytes(body, "previous_response_id").String(); got != "resp_previous" {
					t.Errorf("verification failure must not recover the continuation anchor: %s", body)
				}
				if tc.eventType == "" {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Request-Id", "req_encrypted")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, errorBody)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.outputStarted {
					_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
				}
				if tc.eventType == "error" {
					_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"error\":"+errorObject+"}\n\n")
				} else {
					_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":"+errorObject+"}}\n\n")
				}
			}))
			defer server.Close()

			gateway := &OpenAIGateway{logger: slog.Default(), transportPool: NewTransportPool()}
			ctx := sdk.WithLogger(t.Context(), slog.Default())
			// A later client request must still carry the ciphertext and reach
			// upstream; the verification failure must not populate any cache.
			for attempt := int32(1); attempt <= 2; attempt++ {
				writer := httptest.NewRecorder()
				req := encryptedContentRetryTestRequest(ciphertext, "")
				req.Account = &sdk.Account{ID: 1, Credentials: map[string]string{
					"api_key":  "sk-test",
					"base_url": server.URL,
				}}
				req.Body = append([]byte(`{"previous_response_id":"resp_previous",`), req.Body[1:]...)
				req.Headers.Set("X-Forwarded-Path", "/v1/responses")
				req.DispatchPlan = sdk.DispatchPlan{SchedulingModel: req.Model, WireModel: req.Model}
				req.Stream = tc.eventType != ""
				req.Writer = writer

				outcome, err := gateway.Forward(ctx, req)
				if err != nil {
					t.Fatalf("verification failure returned transport error: %v", err)
				}
				if outcome.Kind != sdk.OutcomeClientError || outcome.FailoverScope != sdk.FailoverScopeNone || outcome.SafetyRejected {
					t.Fatalf("verification failure must be terminal without account penalty: %+v", outcome)
				}
				if outcome.Upstream.StatusCode != http.StatusBadRequest || string(outcome.Upstream.Body) != errorBody {
					t.Fatalf("verification error details changed: %+v", outcome.Upstream)
				}
				if tc.eventType == "" && outcome.Upstream.Headers.Get("X-Request-Id") != "req_encrypted" {
					t.Fatal("HTTP upstream request ID was not preserved")
				}
				if tc.outputStarted {
					if !strings.Contains(writer.Body.String(), "data: "+errorBody+"\n\n") {
						t.Fatalf("started stream lost terminal error details: %s", writer.Body.String())
					}
				} else if tc.eventType != "" && writer.Body.Len() != 0 {
					t.Fatalf("unstarted stream must let Core return the 400 error: %s", writer.Body.String())
				}
				if got := requestCount.Load(); got != attempt {
					t.Fatalf("%d client requests made %d upstream calls", attempt, got)
				}
			}
		})
	}
}

func TestSafetyRejectionEncryptedContentCacheExpires(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest()
	gateway := &OpenAIGateway{}
	encryptedContentCacheForTest(gateway).ttl = time.Nanosecond
	req := encryptedContentRetryTestRequest(valid, "")
	state := preprocessEncryptedContentRetryTestRequest(gateway, req)
	if !state.CacheViolation(explicitUpstreamError{Code: promptUsagePolicyErrorCode}) {
		t.Fatal("expected retry marker to be cached")
	}
	time.Sleep(time.Millisecond)

	retry := encryptedContentRetryTestRequest(valid, "")
	retryState := preprocessEncryptedContentRetryTestRequest(gateway, retry)
	if retryState.sanitized {
		t.Fatal("expired retry marker should not sanitize request")
	}
}

func TestIsInvalidEncryptedContentOutcome(t *testing.T) {
	outcome := sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Body:       []byte(`{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified."}}`),
		},
	}
	if !isInvalidEncryptedContentOutcome(outcome, errors.New("upstream request rejected")) {
		t.Fatal("invalid encrypted content outcome was not detected")
	}
	success := sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK, Body: outcome.Upstream.Body},
	}
	if isInvalidEncryptedContentOutcome(success, nil) {
		t.Fatal("successful assistant output must not be classified as invalid encrypted content")
	}
}
