package gateway

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type gatewayErrReader struct{}

func (gatewayErrReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestAccountUsageWindowHelpers(t *testing.T) {
	if got := resetAtFromBase(time.Time{}, 0); got != nil {
		t.Fatalf("non-positive reset should be nil, got %#v", got)
	}
	base := time.Date(2026, 6, 20, 1, 0, 0, 0, time.UTC)
	reset := resetAtFromBase(base, 90)
	if reset == nil || !reset.Equal(base.Add(90*time.Second)) {
		t.Fatalf("resetAtFromBase = %#v", reset)
	}
	if reset := resetAtFromBase(time.Time{}, 1); reset == nil || reset.Before(time.Now().Add(-time.Second)) {
		t.Fatalf("zero base reset should use current time, got %#v", reset)
	}

	now := base
	win := newAccountUsageWindow("model:5h:spark", "5h spark", 42.5, reset, now)
	if win.Slot != "5h" || win.Group != "model:spark" || win.DisplayLabel != "5h" {
		t.Fatalf("model window labels = %#v", win)
	}
	if win.ResetAfterSeconds != 90 || win.ResetAt == "" || win.UpdatedAt != now.Format(time.RFC3339) {
		t.Fatalf("window reset/update fields = %#v", win)
	}
	past := base.Add(-time.Second)
	win = newAccountUsageWindow("custom", "Custom Window", 1, &past, now)
	if win.Slot != "custom" || win.Group != "base" || win.ResetAfterSeconds != 0 {
		t.Fatalf("custom window = %#v", win)
	}

	if got := usageWindowSlot("abc:7d:def", "ignored"); got != "7d" {
		t.Fatalf("7d slot = %q", got)
	}
	if got := usageWindowSlot("abc", "5h model"); got != "5h" {
		t.Fatalf("5h label slot = %q", got)
	}
	if got := usageWindowGroup("custom", "7d GPT 5", "7d"); got != "model:gpt-5" {
		t.Fatalf("label model group = %q", got)
	}
}

func TestOperationPolicyHelpers(t *testing.T) {
	if _, denied := enforceOperationPolicies(nil, "/v1/images/generations"); denied {
		t.Fatal("nil request should not be denied")
	}

	req := &sdk.ForwardRequest{Headers: http.Header{}, Body: []byte(`{"tools":[{"type":"image_generation"}]}`)}
	outcome, denied := enforceOperationPolicies(req, "/v1/images/generations?x=1")
	if !denied || outcome.Kind != sdk.OutcomeClientError || outcome.Upstream.StatusCode != http.StatusForbidden {
		t.Fatalf("image generation denied outcome = %#v denied=%v", outcome, denied)
	}
	if code := gjson.GetBytes(outcome.Upstream.Body, "error.code").String(); code != "images_generate_disabled" {
		t.Fatalf("denied code = %q", code)
	}

	req.Headers.Set("X-Airgate-Operation-Images-Generate", " TRUE ")
	if _, denied := enforceOperationPolicies(req, "/v1/images/generations/"); denied {
		t.Fatal("enabled image generation should pass")
	}

	req.Headers = http.Header{}
	outcome, denied = enforceOperationPolicies(req, "/images/edits")
	if !denied || gjson.GetBytes(outcome.Upstream.Body, "error.code").String() != "images_edit_disabled" {
		t.Fatalf("image edit denied = %#v denied=%v", outcome, denied)
	}

	req.Headers.Set("X-Airgate-Operation-Images-Edit", "true")
	if _, denied := enforceOperationPolicies(req, "/images/edits"); denied {
		t.Fatal("enabled image edit should pass")
	}

	req.Headers = http.Header{}
	outcome, denied = enforceOperationPolicies(req, "/v1/responses")
	if !denied || gjson.GetBytes(outcome.Upstream.Body, "error.code").String() != "responses_image_generation_disabled" {
		t.Fatalf("responses image tool denied = %#v denied=%v", outcome, denied)
	}
	req.Headers.Set("X-Airgate-Operation-Responses-Image-Generation", "true")
	if _, denied := enforceOperationPolicies(req, "/responses"); denied {
		t.Fatal("enabled responses image tool should pass")
	}
	req.Body = []byte(`{"tools":[{"type":"web_search"}]}`)
	req.Headers = http.Header{}
	if _, denied := enforceOperationPolicies(req, "/v1/responses"); denied {
		t.Fatal("responses without image_generation tool should pass")
	}

	if operationEnabled(nil, "images.generate") || operationEnabled(http.Header{}, "") {
		t.Fatal("operationEnabled should reject nil headers and blank operation")
	}
	if got := canonicalOperationHeader("responses_image-generation"); got != "Responses-Image-Generation" {
		t.Fatalf("canonicalOperationHeader = %q", got)
	}
	if hasResponsesImageGenerationTool(nil) || hasResponsesImageGenerationTool([]byte(`{"tools":[{"type":"IMAGE_GENERATION"}]}`)) == false {
		t.Fatal("hasResponsesImageGenerationTool returned unexpected result")
	}
	if normalizeForwardedPath(" /V1/Responses/?x=1 ") != "/v1/responses" {
		t.Fatal("normalizeForwardedPath should trim, lower, strip query and slash")
	}
	if isImagesGenerationsPath("/images/generations") == false || isImagesEditsPath("/v1/images/edits") == false {
		t.Fatal("image path helpers should recognize aliases")
	}
}

func TestErrorJSONAndClassificationHelpers(t *testing.T) {
	for status, want := range map[int]string{
		400: "invalid_request_error",
		401: "authentication_error",
		403: "permission_error",
		404: "not_found_error",
		422: "invalid_model_error",
		429: "rate_limit_error",
		529: "overloaded_error",
		500: "api_error",
	} {
		if got := anthropicErrorType(status); got != want {
			t.Fatalf("anthropicErrorType(%d) = %q, want %q", status, got, want)
		}
	}
	body := anthropicErrorJSON("invalid_request_error", "bad request")
	if gjson.GetBytes(body, "error.type").String() != "invalid_request_error" ||
		gjson.GetBytes(body, "error.message").String() != "bad request" {
		t.Fatalf("anthropic error body = %s", body)
	}
	body = anthropicErrorJSONWithCode("permission_error", "safety_rejected", "blocked")
	if gjson.GetBytes(body, "error.code").String() != "safety_rejected" {
		t.Fatalf("anthropic error code body = %s", body)
	}

	if got := openAIErrorTypeForStatus(http.StatusTooManyRequests); got != "rate_limit_error" {
		t.Fatalf("429 error type = %q", got)
	}
	if got := openAIErrorTypeForStatus(http.StatusBadGateway); got != "server_error" {
		t.Fatalf("5xx error type = %q", got)
	}
	if got := openAIErrorTypeForStatus(http.StatusBadRequest); got != "invalid_request_error" {
		t.Fatalf("4xx error type = %q", got)
	}

	if got := classifyAnthropicBody(http.StatusBadRequest, []byte(`{"error":{"message":"usage limit reached"}}`)); got != sdk.OutcomeAccountRateLimited {
		t.Fatalf("classifyAnthropicBody = %v", got)
	}
	if got := classifyAnthropicBody(http.StatusForbidden, []byte(`plain disabled account`)); got != sdk.OutcomeAccountDead {
		t.Fatalf("classifyAnthropicBody plain = %v", got)
	}
	if !isTemporaryRateLimitText("try again in 1s") || isTemporaryRateLimitText("") {
		t.Fatal("rate limit text helper unexpected")
	}
	if !isDisabledAccountText("account suspended") || isDisabledAccountText("") {
		t.Fatal("disabled account text helper unexpected")
	}
	for _, msg := range []string{"model_not_found", "this model does not exist", "invalid model"} {
		if !isModelUnsupportedText(msg) {
			t.Fatalf("model unsupported text not detected: %q", msg)
		}
	}
	if isModelUnsupportedText("feature not supported") {
		t.Fatal("model keyword should be required for generic unsupported text")
	}

	if got := extractRetryAfterHeader(http.Header{"Retry-After": []string{"2"}}); got != 2*time.Second {
		t.Fatalf("Retry-After duration = %v", got)
	}
	if got := extractRetryAfterHeader(http.Header{}); got != 0 {
		t.Fatalf("empty Retry-After = %v", got)
	}
	if got := truncate("abcdef", 10); got != "abcdef" {
		t.Fatalf("short truncate = %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc..." {
		t.Fatalf("long truncate = %q", got)
	}
	if _, err := readLimitedErrorBody(gatewayErrReader{}); err == nil {
		t.Fatal("readLimitedErrorBody should return reader error")
	}
	data, err := readLimitedErrorBody(io.LimitReader(strings.NewReader("abcdef"), 3))
	if err != nil || string(data) != "abc" {
		t.Fatalf("readLimitedErrorBody data=%q err=%v", data, err)
	}
}

func TestOutcomeConstructorsAndPolicies(t *testing.T) {
	headers := http.Header{"X-Test": []string{"ok"}}
	usage := &sdk.Usage{Model: "gpt-5.4", InputTokens: 10, OutputTokens: 5}
	outcome := successOutcome(http.StatusCreated, []byte(`{"ok":true}`), headers, usage)
	if outcome.Kind != sdk.OutcomeSuccess || outcome.Upstream.StatusCode != http.StatusCreated || outcome.Usage != usage {
		t.Fatalf("successOutcome = %#v", outcome)
	}

	outcome = failureOutcome(http.StatusBadRequest, []byte("bad model"), headers, "model_not_found", 0)
	if outcome.Kind != sdk.OutcomeClientError || outcome.FailoverScope != sdk.FailoverScopeDispatchCandidate {
		t.Fatalf("failureOutcome model fallback = %#v", outcome)
	}
	if err := forwardErrForOutcome(outcome, errors.New("client")); err != nil {
		t.Fatalf("client error should be swallowed, got %v", err)
	}
	outcome = failureOutcome(http.StatusTooManyRequests, []byte("rate limit"), headers, "rate limit", time.Second)
	if outcome.Kind != sdk.OutcomeAccountRateLimited || outcome.RetryAfter != time.Second {
		t.Fatalf("failureOutcome rate limit = %#v", outcome)
	}
	applyImageRateLimitPolicy(&outcome)
	if outcome.RetryAfter != imageRateLimitMinRetryAfter {
		t.Fatalf("image rate limit RetryAfter = %v", outcome.RetryAfter)
	}
	outcome = failureOutcome(http.StatusTooManyRequests, []byte("overloaded"), headers, "Our servers are currently overloaded. Please try again later.", 0)
	if outcome.Kind != sdk.OutcomeFamilyTransient {
		t.Fatalf("failureOutcome overloaded = %#v", outcome)
	}
	applyImageRateLimitPolicy(nil)
	success := successOutcome(http.StatusOK, nil, nil, nil)
	applyImageRateLimitPolicy(&success)

	transient := transientOutcome("network failed")
	if transient.Kind != sdk.OutcomeUpstreamTransient || transient.Upstream.StatusCode != http.StatusBadGateway {
		t.Fatalf("transientOutcome = %#v", transient)
	}
	dead := accountDeadOutcome("missing token")
	if dead.Kind != sdk.OutcomeAccountDead || dead.Upstream.StatusCode != http.StatusUnauthorized {
		t.Fatalf("accountDeadOutcome = %#v", dead)
	}
	if err := forwardErrForOutcome(dead, errors.New("dead")); err == nil {
		t.Fatal("non-client outcome should preserve error")
	}
}

func TestCodexUsageSSEAndProbeHelpers(t *testing.T) {
	if got := parseCodexUsageFromSSEEvent([]byte(`bad`)); got != nil {
		t.Fatalf("bad SSE event parsed: %#v", got)
	}
	if got := parseCodexUsageFromSSEEvent([]byte(`{"rate_limits":{"primary":{"used_percent":0},"secondary":{"used_percent":0}}}`)); got != nil {
		t.Fatalf("empty SSE usage parsed: %#v", got)
	}
	snapshot := parseCodexUsageFromSSEEvent([]byte(`{"rate_limits":{"primary":{"used_percent":12.5,"reset_after_seconds":30,"window_minutes":300},"secondary":{"used_percent":75,"reset_after_seconds":600,"window_minutes":10080}}}`))
	if snapshot == nil || snapshot.PrimaryUsedPercent != 12.5 || snapshot.SecondaryWindowMinutes != 10080 || snapshot.CapturedAt.IsZero() {
		t.Fatalf("SSE usage snapshot = %#v", snapshot)
	}

	StoreProbeError(12345, "HTTP 401")
	if got := GetProbeError(12345); got != "HTTP 401" {
		t.Fatalf("probe error = %q", got)
	}
	if got := GetProbeError(12345); got != "" {
		t.Fatalf("probe error should be consumed, got %q", got)
	}

	for _, headers := range []http.Header{
		{"User-Agent": []string{"codex-cli/1.0"}},
		{"Originator": []string{"codex_cli_rs"}},
		{"X-Stainless-Timeout": []string{"30"}},
	} {
		if !isCodexCLI(headers) {
			t.Fatalf("isCodexCLI false for %#v", headers)
		}
	}
	if isCodexCLI(http.Header{"User-Agent": []string{"curl"}}) {
		t.Fatal("curl should not be Codex CLI")
	}
}

func TestTransportPoolHelpers(t *testing.T) {
	if got := poolKey(12, ""); got != "direct:12" {
		t.Fatalf("direct pool key = %q", got)
	}
	if got := poolKey(-12, "http://proxy"); got != "proxy:http://proxy:-12" {
		t.Fatalf("proxy pool key = %q", got)
	}
	for n, want := range map[int64]string{0: "0", 7: "7", -7: "-7", 123456789: "123456789"} {
		if got := itoa(n); got != want {
			t.Fatalf("itoa(%d) = %q, want %q", n, got, want)
		}
	}

	pool := NewTransportPool()
	first := pool.GetTransport(1, "")
	if first == nil {
		t.Fatal("GetTransport returned nil")
	}
	if second := pool.GetTransport(1, ""); second != first {
		t.Fatal("same account/proxy should reuse transport")
	}
	if other := pool.GetTransport(1, "http://proxy.example:8080"); other == first || other.Proxy == nil {
		t.Fatal("different proxy should create proxied transport")
	}
	if invalid := pool.GetTransport(2, "://bad"); invalid == nil {
		t.Fatal("invalid proxy URL should still create transport")
	}

	oldKey := poolKey(99, "")
	pool.mu.Lock()
	pool.transports[oldKey] = &transportPoolEntry{transport: &http.Transport{}, lastUsedAt: time.Now().Add(-transportPoolIdleTTL - time.Minute)}
	pool.lastCleanupTime = time.Now().Add(-transportPoolCleanupInterval - time.Minute)
	pool.cleanupIdleLocked(time.Now())
	_, exists := pool.transports[oldKey]
	pool.mu.Unlock()
	if exists {
		t.Fatal("cleanupIdleLocked should remove old entry")
	}

	pool.mu.Lock()
	pool.transports["newer"] = &transportPoolEntry{transport: &http.Transport{}, lastUsedAt: time.Now()}
	pool.transports["older"] = &transportPoolEntry{transport: &http.Transport{}, lastUsedAt: time.Now().Add(-time.Hour)}
	pool.deleteOldestLocked()
	_, olderExists := pool.transports["older"]
	_, newerExists := pool.transports["newer"]
	pool.mu.Unlock()
	if olderExists || !newerExists {
		t.Fatal("deleteOldestLocked should remove only the oldest entry")
	}

	pool.GetTransport(42, "")
	pool.GetTransport(42, "http://proxy.example:8080")
	pool.RemoveAccount(42)
	if _, ok := pool.transports[poolKey(42, "")]; ok {
		t.Fatal("RemoveAccount should remove direct transport")
	}
	if _, ok := pool.transports[poolKey(42, "http://proxy.example:8080")]; ok {
		t.Fatal("RemoveAccount should remove proxied transport")
	}
	pool.CloseIdle()
}
