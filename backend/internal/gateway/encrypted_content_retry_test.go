package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"github.com/tidwall/gjson"
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

func TestInvalidEncryptedContentRetryMatchesCiphertextAcrossBodyChanges(t *testing.T) {
	rejected := validGPTReasoningEncryptedContentForTestMarker(0x11)
	fresh := validGPTReasoningEncryptedContentForTestMarker(0x22)
	gateway := &OpenAIGateway{}
	first := encryptedContentRetryTestRequest(rejected, "")

	firstState := preprocessEncryptedContentRetryTestRequest(gateway, first)
	if firstState.retrySanitized {
		t.Fatal("first request must preserve encrypted_content")
	}
	if got := gjson.GetBytes(first.Body, "input.0.encrypted_content").String(); got != rejected {
		t.Fatalf("first request encrypted_content = %q, want preserved", got)
	}
	if !gateway.cacheInvalidEncryptedContentRetry(firstState, "/v1/responses") {
		t.Fatal("invalid encrypted content failure should cache the ciphertext hash")
	}

	retry := &sdk.ForwardRequest{Body: []byte(`{"model":"gpt-5.4","input":[` +
		`{"id":"rs_rejected","type":"reasoning","encrypted_content":"` + rejected + `","summary":[]},` +
		`{"id":"rs_fresh","type":"reasoning","encrypted_content":"` + fresh + `","summary":[]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue with newly added text"}]}` +
		`]}`)}
	retryState := preprocessEncryptedContentRetryTestRequest(gateway, retry)
	if !retryState.retrySanitized {
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
	if freshState.retrySanitized {
		t.Fatal("an uncached ciphertext must not be removed")
	}
	if got := gjson.GetBytes(requestWithFreshCiphertext.Body, "input.0.encrypted_content").String(); got != fresh {
		t.Fatalf("fresh encrypted_content = %q, want preserved", got)
	}
}

func TestInvalidEncryptedContentRetryCachesOnlyValidReasoningCiphertexts(t *testing.T) {
	gateway := &OpenAIGateway{}
	for _, body := range [][]byte{
		[]byte(`{"input":[{"type":"reasoning","encrypted_content":"bad"}]}`),
		[]byte(`{"input":[{"type":"compaction","encrypted_content":"` + validGPTReasoningEncryptedContentForTest() + `"}]}`),
	} {
		req := &sdk.ForwardRequest{Body: body, Model: "gpt-5.4"}
		state := preprocessEncryptedContentRetryTestRequest(gateway, req)
		if gateway.cacheInvalidEncryptedContentRetry(state, "/v1/responses") {
			t.Fatalf("invalid or non-reasoning encrypted content was cached: %s", body)
		}
	}
}

func TestForwardInvalidEncryptedContentOnlySanitizesClientRetry(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest()
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attempt := requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			if !gjson.GetBytes(body, "input.0.encrypted_content").Exists() {
				t.Errorf("first upstream request removed encrypted_content: %s", body)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified."}}`))
			return
		}
		if gjson.GetBytes(body, "input.0.encrypted_content").Exists() || gjson.GetBytes(body, "input.0.id").Exists() {
			t.Errorf("client retry retained encrypted reasoning data: %s", body)
		}
		if got := gjson.GetBytes(body, "input.0.summary.0.text").String(); got != "keep" {
			t.Errorf("client retry summary = %q, want keep; body=%s", got, body)
		}
		_, _ = w.Write([]byte(`{"id":"resp_retry","object":"response","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	newRequest := func(suffix string) *sdk.ForwardRequest {
		return &sdk.ForwardRequest{
			Account: &sdk.Account{ID: 1, Credentials: map[string]string{
				"api_key":  "sk-test",
				"base_url": server.URL,
			}},
			Body: []byte(`{"model":"gpt-5.4","input":[` +
				`{"id":"rs_retry","type":"reasoning","encrypted_content":"` + valid + `","summary":[{"type":"summary_text","text":"keep"}]},` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue` + suffix + `"}]}` +
				`]}`),
			Headers: http.Header{
				"Content-Type":     []string{"application/json"},
				"X-Forwarded-Path": []string{"/v1/responses"},
			},
			Model:        "gpt-5.4",
			DispatchPlan: sdk.DispatchPlan{SchedulingModel: "gpt-5.4", WireModel: "gpt-5.4"},
		}
	}

	gateway := &OpenAIGateway{logger: slog.Default(), transportPool: NewTransportPool()}
	ctx := sdk.WithLogger(context.Background(), slog.Default())
	firstOutcome, firstErr := gateway.Forward(ctx, newRequest(""))
	if !isInvalidEncryptedContentOutcome(firstOutcome, firstErr) {
		t.Fatalf("first request outcome=%#v err=%v, want invalid encrypted content", firstOutcome, firstErr)
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("first client request made %d upstream attempts, want exactly 1", got)
	}

	retryOutcome, retryErr := gateway.Forward(ctx, newRequest(" with added retry content"))
	if retryErr != nil {
		t.Fatalf("client retry returned error: %v", retryErr)
	}
	if retryOutcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("client retry outcome = %s, want success; reason=%s", retryOutcome.Kind, retryOutcome.Reason)
	}
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("two client requests made %d upstream attempts, want exactly 2", got)
	}
}

func TestForwardStreamingInvalidEncryptedContentOnlySanitizesClientRetry(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest()
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attempt := requestCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if attempt == 1 {
			if !gjson.GetBytes(body, "input.0.encrypted_content").Exists() {
				t.Errorf("first streaming request removed encrypted_content: %s", body)
			}
			_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content gAAA...9Q== could not be verified. Reason: Encrypted content could not be decrypted or parsed."}}}`+"\n\n")
			return
		}
		if gjson.GetBytes(body, "input.0.encrypted_content").Exists() || gjson.GetBytes(body, "input.0.id").Exists() {
			t.Errorf("streaming client retry retained encrypted reasoning data: %s", body)
		}
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"ok"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_retry","model":"gpt-5.4","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer server.Close()

	newRequest := func(suffix string) *sdk.ForwardRequest {
		return &sdk.ForwardRequest{
			Account: &sdk.Account{ID: 1, Credentials: map[string]string{
				"api_key":  "sk-test",
				"base_url": server.URL,
			}},
			Body: []byte(`{"model":"gpt-5.4","stream":true,"input":[` +
				`{"id":"rs_retry","type":"reasoning","encrypted_content":"` + valid + `","summary":[{"type":"summary_text","text":"keep"}]},` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue` + suffix + `"}]}` +
				`]}`),
			Headers: http.Header{
				"Content-Type":     []string{"application/json"},
				"X-Forwarded-Path": []string{"/v1/responses"},
			},
			Model:        "gpt-5.4",
			DispatchPlan: sdk.DispatchPlan{SchedulingModel: "gpt-5.4", WireModel: "gpt-5.4"},
			Stream:       true,
			Writer:       httptest.NewRecorder(),
		}
	}

	gateway := &OpenAIGateway{logger: slog.Default(), transportPool: NewTransportPool()}
	ctx := sdk.WithLogger(context.Background(), slog.Default())
	firstOutcome, firstErr := gateway.Forward(ctx, newRequest(""))
	if !isInvalidEncryptedContentOutcome(firstOutcome, firstErr) {
		t.Fatalf("first streaming request outcome=%#v err=%v, want invalid encrypted content", firstOutcome, firstErr)
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("first streaming client request made %d upstream attempts, want exactly 1", got)
	}

	retryOutcome, retryErr := gateway.Forward(ctx, newRequest(" with added retry content"))
	if retryErr != nil {
		t.Fatalf("streaming client retry returned error: %v", retryErr)
	}
	if retryOutcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("streaming client retry outcome = %s, want success; reason=%s", retryOutcome.Kind, retryOutcome.Reason)
	}
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("two streaming client requests made %d upstream attempts, want exactly 2", got)
	}
}

func TestInvalidEncryptedContentRetryCacheExpires(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest()
	gateway := &OpenAIGateway{encryptedContentRetry: safetyRequestCache{ttl: time.Nanosecond}}
	req := encryptedContentRetryTestRequest(valid, "")
	state := preprocessEncryptedContentRetryTestRequest(gateway, req)
	if !gateway.cacheInvalidEncryptedContentRetry(state, "/v1/responses") {
		t.Fatal("expected retry marker to be cached")
	}
	time.Sleep(time.Millisecond)

	retry := encryptedContentRetryTestRequest(valid, "")
	retryState := preprocessEncryptedContentRetryTestRequest(gateway, retry)
	if retryState.retrySanitized {
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

func TestInvalidEncryptedContentRetryCacheDefaults(t *testing.T) {
	if invalidEncryptedContentRetryCacheTTL != 24*time.Hour {
		t.Fatalf("retry cache TTL = %s, want 24h", invalidEncryptedContentRetryCacheTTL)
	}
	if invalidEncryptedContentRetryCacheMaxEntries != 100_000 {
		t.Fatalf("retry cache capacity = %d, want 100000", invalidEncryptedContentRetryCacheMaxEntries)
	}
}

func TestInvalidEncryptedContentRetryCacheCapacity(t *testing.T) {
	hashes := make([]uint64, invalidEncryptedContentRetryCacheMaxEntries+1)
	for index := range hashes {
		hashes[index] = uint64(index + 1)
	}
	var cache safetyRequestCache
	now := time.Now()
	cache.addHashesWithLimits(
		hashes,
		now,
		invalidEncryptedContentRetryCacheTTL,
		invalidEncryptedContentRetryCacheMaxEntries,
	)
	size, capacity := cache.statsWithCapacity(now, invalidEncryptedContentRetryCacheMaxEntries)
	if capacity != invalidEncryptedContentRetryCacheMaxEntries {
		t.Fatalf("retry cache capacity = %d, want %d", capacity, invalidEncryptedContentRetryCacheMaxEntries)
	}
	if size != invalidEncryptedContentRetryCacheMaxEntries {
		t.Fatalf("retry cache size = %d, want capped at %d", size, invalidEncryptedContentRetryCacheMaxEntries)
	}
}
