package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const promptUsagePolicyRejectionForTest = "Invalid prompt: your prompt was flagged as potentially violating our usage policy. Please try again with a different prompt: https://platform.openai.com/docs/guides/reasoning#advice-on-prompting"

func promptPolicyTestRequest(accountType, first, second, suffix string) *sdk.ForwardRequest {
	return &sdk.ForwardRequest{
		Account: &sdk.Account{Type: accountType},
		Body: []byte(`{"model":"gpt-5.6-luna","input":[` +
			`{"id":"rs_first","type":"reasoning","encrypted_content":"` + first + `","summary":[{"type":"summary_text","text":"keep first"}]},` +
			`{"id":"rs_second","type":"reasoning","encrypted_content":"` + second + `","summary":[{"type":"summary_text","text":"keep second"}]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue` + suffix + `"}]}` +
			`]}`),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Model:   "gpt-5.6-luna",
	}
}

func promptUsagePolicyFailureOutcomeForTest() sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Body: []byte(`{"error":{"type":"invalid_request_error","code":"invalid_prompt","message":"` +
				promptUsagePolicyRejectionForTest + `"}}`),
		},
		Reason: promptUsagePolicyRejectionForTest,
	}
}

func cybersecurityRiskFailureOutcomeForTest() sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Body: []byte(`{"error":{"type":"invalid_request_error","code":"cybersecurity_risk","message":"` +
				cybersecurityRiskMessage + `"}}`),
		},
		Reason:         cybersecurityRiskMessage,
		SafetyRejected: true,
	}
}

func invalidEncryptedContentFailureOutcomeForTest(rejected string) sdk.ForwardOutcome {
	marker := abbreviatedEncryptedContentForTest(rejected)
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Body: []byte(`{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content ` +
				marker + ` could not be verified."}}`),
		},
		Reason: "The encrypted content " + marker + " could not be verified.",
	}
}

func preprocessPromptPolicyTestRequest(begin textHashBegin, req *sdk.ForwardRequest) encryptedContentHashSession {
	session := begin.request.EncryptedContent()
	req.Body = preprocessRequestBodyWithEncryptedContentState(
		req.Body,
		req.Model,
		"/v1/responses",
		session,
		req.Headers,
	)
	return session
}

func TestPromptUsagePolicyCachesPromptAndEncryptedContent(t *testing.T) {
	first := validGPTReasoningEncryptedContentForTestMarker(0x31)
	second := validGPTReasoningEncryptedContentForTestMarker(0x32)
	hash := &enabledTextHash{}
	req := promptPolicyTestRequest("apikey", first, second, "")

	begin := hash.Begin(req, http.MethodPost, "/v1/responses", "")
	if begin.outcome != nil || begin.event != textHashBeginContinue {
		t.Fatalf("first request begin = %+v", begin)
	}
	preprocessPromptPolicyTestRequest(begin, req)
	if got := gjson.GetBytes(req.Body, "input.0.encrypted_content").String(); got != first {
		t.Fatalf("first encrypted_content = %q, want preserved", got)
	}
	if got := gjson.GetBytes(req.Body, "input.1.encrypted_content").String(); got != second {
		t.Fatalf("second encrypted_content = %q, want preserved", got)
	}

	finish := begin.request.Finish(promptUsagePolicyFailureOutcomeForTest(), nil)
	if !finish.promptSafetyCached || !finish.textSafetyCached || !finish.encryptedContentCached {
		t.Fatalf("finish = %+v, want prompt and encrypted content cached", finish)
	}
	if size, _ := hash.promptSafety.stats(time.Now()); size != 1 {
		t.Fatalf("prompt rejection cache size = %d, want 1", size)
	}
	if size, _ := hash.cyberSafety.stats(time.Now()); size != 0 {
		t.Fatalf("cyber rejection cache size = %d, want 0", size)
	}
	requestSize, _, requestCounts := hash.textSafety.statsWithCategoryCounts(
		time.Now(),
		textSafetyRequestCacheMaxEntries,
		cybersecurityRiskErrorCode,
		promptUsagePolicyErrorCode,
	)
	if requestSize != 1 || requestCounts[0] != 0 || requestCounts[1] != 1 {
		t.Fatalf("request rejection stats = size:%d counts:%v, want invalid_prompt only", requestSize, requestCounts)
	}
	scope := encryptedContentScopeHash(req)
	if !hash.encryptedContent.contains(encryptedContentHashWithScope(scope, first), time.Now()) {
		t.Fatal("prompt rejection must cache first encrypted_content")
	}
	if !hash.encryptedContent.contains(encryptedContentHashWithScope(scope, second), time.Now()) {
		t.Fatal("prompt rejection must cache second encrypted_content")
	}

	retry := promptPolicyTestRequest("apikey", validGPTReasoningEncryptedContentForTestMarker(0x33), validGPTReasoningEncryptedContentForTestMarker(0x34), "")
	retryBegin := hash.Begin(retry, http.MethodPost, "/v1/responses", "")
	if retryBegin.outcome == nil || retryBegin.event != textHashBeginPromptSafetyCacheHit {
		t.Fatalf("complete retry begin = %+v, want prompt policy cache hit", retryBegin)
	}
	if retryBegin.outcome.Upstream.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("complete retry status = %d, want 429", retryBegin.outcome.Upstream.StatusCode)
	}
}

func TestPromptUsagePolicyRemovesRejectedEncryptedContent(t *testing.T) {
	rejectedFirst := validGPTReasoningEncryptedContentForTestMarker(0x41)
	rejectedSecond := validGPTReasoningEncryptedContentForTestMarker(0x42)
	fresh := validGPTReasoningEncryptedContentForTestMarker(0x43)
	otherFresh := validGPTReasoningEncryptedContentForTestMarker(0x44)
	hash := &enabledTextHash{}

	firstReq := promptPolicyTestRequest("oauth", rejectedFirst, rejectedSecond, "")
	firstBegin := hash.Begin(firstReq, http.MethodPost, "/v1/responses", "")
	preprocessPromptPolicyTestRequest(firstBegin, firstReq)
	firstBegin.request.Finish(promptUsagePolicyFailureOutcomeForTest(), nil)

	changedReq := promptPolicyTestRequest("oauth", rejectedFirst, fresh, " with changed visible input")
	changedBegin := hash.Begin(changedReq, http.MethodPost, "/v1/responses", "")
	if changedBegin.outcome != nil {
		t.Fatalf("changed request unexpectedly hit complete request cache: %+v", changedBegin.outcome)
	}
	changedSession := preprocessPromptPolicyTestRequest(changedBegin, changedReq)
	if !changedSession.Sanitized() {
		t.Fatal("prompt rejection must sanitize encrypted_content")
	}
	if gjson.GetBytes(changedReq.Body, "input.0.encrypted_content").Exists() {
		t.Fatalf("changed request retained rejected encrypted_content: %s", changedReq.Body)
	}
	if got := gjson.GetBytes(changedReq.Body, "input.1.encrypted_content").String(); got != fresh {
		t.Fatalf("changed request removed fresh encrypted_content: got %q, want %q", got, fresh)
	}
	if got := gjson.GetBytes(changedReq.Body, "input.0.summary.0.text").String(); got != "keep first" {
		t.Fatalf("first summary = %q, want preserved", got)
	}
	if got := gjson.GetBytes(changedReq.Body, "input.1.summary.0.text").String(); got != "keep second" {
		t.Fatalf("second summary = %q, want preserved", got)
	}

	freshReq := promptPolicyTestRequest("oauth", fresh, otherFresh, " with wholly fresh ciphertexts")
	freshBegin := hash.Begin(freshReq, http.MethodPost, "/v1/responses", "")
	freshSession := preprocessPromptPolicyTestRequest(freshBegin, freshReq)
	if freshSession.Sanitized() {
		t.Fatal("request with only fresh ciphertexts must remain unchanged")
	}
	if got := gjson.GetBytes(freshReq.Body, "input.0.encrypted_content").String(); got != fresh {
		t.Fatalf("fresh first encrypted_content = %q, want preserved", got)
	}
	if got := gjson.GetBytes(freshReq.Body, "input.1.encrypted_content").String(); got != otherFresh {
		t.Fatalf("fresh second encrypted_content = %q, want preserved", got)
	}
}

func TestPromptUsagePolicyCacheIsScopedByAccountType(t *testing.T) {
	rejected := validGPTReasoningEncryptedContentForTestMarker(0x51)
	second := validGPTReasoningEncryptedContentForTestMarker(0x52)
	fresh := validGPTReasoningEncryptedContentForTestMarker(0x53)
	hash := &enabledTextHash{}

	oauthReq := promptPolicyTestRequest("oauth", rejected, second, "")
	oauthBegin := hash.Begin(oauthReq, http.MethodPost, "/v1/responses", "")
	preprocessPromptPolicyTestRequest(oauthBegin, oauthReq)
	oauthBegin.request.Finish(promptUsagePolicyFailureOutcomeForTest(), nil)

	apiKeyExactRetry := promptPolicyTestRequest("apikey", rejected, second, "")
	apiKeyExactRetryBegin := hash.Begin(apiKeyExactRetry, http.MethodPost, "/v1/responses", "")
	if apiKeyExactRetryBegin.outcome != nil {
		t.Fatalf("API Key request must not share OAuth prompt cache scope: %+v", apiKeyExactRetryBegin)
	}

	apiKeyChanged := promptPolicyTestRequest("apikey", rejected, fresh, " changed account type")
	apiKeyChangedBegin := hash.Begin(apiKeyChanged, http.MethodPost, "/v1/responses", "")
	if apiKeyChangedBegin.outcome != nil {
		t.Fatalf("changed API Key request unexpectedly hit full request cache: %+v", apiKeyChangedBegin.outcome)
	}
	apiKeyChangedSession := preprocessPromptPolicyTestRequest(apiKeyChangedBegin, apiKeyChanged)
	if apiKeyChangedSession.Sanitized() {
		t.Fatal("API Key request must not remove encrypted_content from an OAuth prompt rejection")
	}
	if got := gjson.GetBytes(apiKeyChanged.Body, "input.0.encrypted_content").String(); got != rejected {
		t.Fatalf("changed API Key request first encrypted_content = %q, want preserved", got)
	}
	if got := gjson.GetBytes(apiKeyChanged.Body, "input.1.encrypted_content").String(); got != fresh {
		t.Fatalf("changed API Key request removed fresh encrypted_content: got %q, want %q", got, fresh)
	}
}

func TestCybersecurityRiskCachesRequestAndEncryptedContent(t *testing.T) {
	rejectedFirst := validGPTReasoningEncryptedContentForTestMarker(0x61)
	rejectedSecond := validGPTReasoningEncryptedContentForTestMarker(0x62)
	fresh := validGPTReasoningEncryptedContentForTestMarker(0x63)
	hash := &enabledTextHash{}

	firstReq := promptPolicyTestRequest("apikey", rejectedFirst, rejectedSecond, " cybersecurity")
	firstBegin := hash.Begin(firstReq, http.MethodPost, "/v1/responses", "")
	preprocessPromptPolicyTestRequest(firstBegin, firstReq)
	finish := firstBegin.request.Finish(cybersecurityRiskFailureOutcomeForTest(), nil)
	if !finish.encryptedContentCached || !finish.textSafetyCached {
		t.Fatalf("cybersecurity finish = %+v, want request and ciphertext cached", finish)
	}
	requestSize, _, requestCounts := hash.textSafety.statsWithCategoryCounts(
		time.Now(),
		textSafetyRequestCacheMaxEntries,
		cybersecurityRiskErrorCode,
		promptUsagePolicyErrorCode,
	)
	if requestSize != 1 || requestCounts[0] != 1 || requestCounts[1] != 0 {
		t.Fatalf("request rejection stats = size:%d counts:%v, want cybersecurity_risk only", requestSize, requestCounts)
	}
	if size, _ := hash.promptSafety.stats(time.Now()); size != 0 {
		t.Fatalf("prompt rejection cache size = %d, want 0", size)
	}
	if size, _ := hash.cyberSafety.stats(time.Now()); size != 1 {
		t.Fatalf("cyber rejection cache size = %d, want 1", size)
	}
	scope := encryptedContentScopeHash(firstReq)
	if !hash.encryptedContent.contains(encryptedContentHashWithScope(scope, rejectedFirst), time.Now()) {
		t.Fatal("cybersecurity rejection must cache first encrypted_content")
	}
	if !hash.encryptedContent.contains(encryptedContentHashWithScope(scope, rejectedSecond), time.Now()) {
		t.Fatal("cybersecurity rejection must cache second encrypted_content")
	}

	exactRetry := promptPolicyTestRequest("apikey", rejectedFirst, rejectedSecond, " cybersecurity")
	exactRetryBegin := hash.Begin(exactRetry, http.MethodPost, "/v1/responses", "")
	if exactRetryBegin.outcome == nil || exactRetryBegin.event != textHashBeginSafetyCacheHit {
		t.Fatalf("cybersecurity exact retry = %+v, want safety cache hit", exactRetryBegin)
	}
	if exactRetryBegin.outcome.Upstream.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("cybersecurity exact retry status = %d, want 429", exactRetryBegin.outcome.Upstream.StatusCode)
	}

	carrierChanged := promptPolicyTestRequest(
		"apikey",
		validGPTReasoningEncryptedContentForTestMarker(0x64),
		validGPTReasoningEncryptedContentForTestMarker(0x65),
		" cybersecurity",
	)
	carrierChanged.Stream = true
	carrierChangedBegin := hash.Begin(carrierChanged, http.MethodPost, "/v1/responses", "")
	if carrierChangedBegin.outcome == nil || carrierChangedBegin.event != textHashBeginCyberSafetyCacheHit {
		t.Fatalf("cybersecurity carrier-changed retry = %+v, want cyber cache hit", carrierChangedBegin)
	}

	changedReq := promptPolicyTestRequest("apikey", rejectedFirst, fresh, " changed cybersecurity request")
	changedBegin := hash.Begin(changedReq, http.MethodPost, "/v1/responses", "")
	if changedBegin.outcome != nil {
		t.Fatalf("changed cybersecurity request unexpectedly hit full request cache: %+v", changedBegin.outcome)
	}
	changedSession := preprocessPromptPolicyTestRequest(changedBegin, changedReq)
	if !changedSession.Sanitized() {
		t.Fatal("cybersecurity rejection must sanitize encrypted_content")
	}
	if gjson.GetBytes(changedReq.Body, "input.0.encrypted_content").Exists() {
		t.Fatalf("changed cybersecurity request retained rejected encrypted_content: %s", changedReq.Body)
	}
	if got := gjson.GetBytes(changedReq.Body, "input.1.encrypted_content").String(); got != fresh {
		t.Fatalf("changed cybersecurity request removed fresh encrypted_content: got %q, want %q", got, fresh)
	}
}

func TestInvalidEncryptedContentPreservesExplicitlyIdentifiedCiphertext(t *testing.T) {
	rejected := validGPTReasoningEncryptedContentForTestMarker(0x71)
	secondRejected := validGPTReasoningEncryptedContentForTestMarker(0x72)
	fresh := validGPTReasoningEncryptedContentForTestMarker(0x73)
	hash := &enabledTextHash{}

	invalidReq := promptPolicyTestRequest("apikey", rejected, secondRejected, " invalid encrypted")
	invalidBegin := hash.Begin(invalidReq, http.MethodPost, "/v1/responses", "")
	preprocessPromptPolicyTestRequest(invalidBegin, invalidReq)
	invalidFinish := invalidBegin.request.Finish(invalidEncryptedContentFailureOutcomeForTest(rejected), nil)
	if invalidFinish.encryptedContentCached {
		t.Fatal("invalid_encrypted_content must not cache even an explicitly identified ciphertext")
	}
	if size, _ := hash.encryptedContent.statsWithCapacity(time.Now(), encryptedContentCacheMaxEntries); size != 0 {
		t.Fatalf("encrypted_content cache size = %d, want 0", size)
	}
	scope := encryptedContentScopeHash(invalidReq)
	if hash.encryptedContent.contains(encryptedContentHashWithScope(scope, secondRejected), time.Now()) {
		t.Fatal("invalid_encrypted_content cached an unrelated ciphertext")
	}

	safetyReq := promptPolicyTestRequest("apikey", rejected, fresh, " shared cache")
	safetyBegin := hash.Begin(safetyReq, http.MethodPost, "/v1/responses", "")
	safetySession := preprocessPromptPolicyTestRequest(safetyBegin, safetyReq)
	if safetySession.Sanitized() {
		t.Fatal("invalid_encrypted_content must not sanitize the next request")
	}
	if got := gjson.GetBytes(safetyReq.Body, "input.0.encrypted_content").String(); got != rejected {
		t.Fatalf("verification failure lost ciphertext: %s", safetyReq.Body)
	}
	if got := gjson.GetBytes(safetyReq.Body, "input.1.encrypted_content").String(); got != fresh {
		t.Fatalf("shared cache removed fresh encrypted_content: got %q, want %q", got, fresh)
	}
	safetyBegin.request.Finish(promptUsagePolicyFailureOutcomeForTest(), nil)
	if size, _ := hash.encryptedContent.statsWithCapacity(time.Now(), encryptedContentCacheMaxEntries); size != 2 {
		t.Fatalf("encrypted_content cache size after prompt rejection = %d, want 2", size)
	}
}

func TestInvalidEncryptedContentWithoutUniqueMarkerDoesNotCacheCandidates(t *testing.T) {
	first := validGPTReasoningEncryptedContentForTestMarker(0x81)
	second := validGPTReasoningEncryptedContentForTestMarker(0x82)
	hash := &enabledTextHash{}
	req := promptPolicyTestRequest("apikey", first, second, " ambiguous encrypted content")
	begin := hash.Begin(req, http.MethodPost, "/v1/responses", "")
	preprocessPromptPolicyTestRequest(begin, req)
	outcome := sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Body:       []byte(`{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified."}}`),
		},
	}
	finish := begin.request.Finish(outcome, nil)
	if finish.encryptedContentCached {
		t.Fatalf("ambiguous invalid_encrypted_content finish = %+v, want no candidate cached", finish)
	}
	if size, _ := hash.encryptedContent.statsWithCapacity(time.Now(), encryptedContentCacheMaxEntries); size != 0 {
		t.Fatalf("ambiguous encrypted_content cache size = %d, want 0", size)
	}
}

func TestEncryptedContentRemovalRequiresExplicitSafetyRejection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        string
		kind        sdk.OutcomeKind
		status      int
		wantRemoval bool
	}{
		{name: "prompt", code: "invalid_prompt", kind: sdk.OutcomeClientError, status: 400, wantRemoval: true},
		{name: "cyber", code: "cybersecurity_risk", kind: sdk.OutcomeClientError, status: 400, wantRemoval: true},
		{name: "cyber alias", code: "cyber_policy", kind: sdk.OutcomeClientError, status: 403, wantRemoval: true},
		{name: "verification", code: "invalid_encrypted_content", kind: sdk.OutcomeClientError, status: 400},
		{name: "generic safety", code: "safety_rejected", kind: sdk.OutcomeClientError, status: 400},
		{name: "content policy", code: "content_policy_violation", kind: sdk.OutcomeClientError, status: 400},
		{name: "rate limit", code: "rate_limit_exceeded", kind: sdk.OutcomeAccountRateLimited, status: 429},
		{name: "server error", code: "server_error", kind: sdk.OutcomeUpstreamTransient, status: 500},
		{name: "success quoting rejection", code: "invalid_prompt", kind: sdk.OutcomeSuccess, status: 200},
		{name: "message only", kind: sdk.OutcomeClientError, status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash := &enabledTextHash{}
			// Opaque strings deliberately do not resemble any assumed envelope.
			req := promptPolicyTestRequest("apikey", "opaque-first", "opaque-second", "")
			begin := hash.Begin(req, http.MethodPost, "/v1/responses", "")
			preprocessPromptPolicyTestRequest(begin, req)
			finish := begin.request.Finish(sdk.ForwardOutcome{
				Kind: tc.kind,
				Upstream: sdk.UpstreamResponse{
					StatusCode: tc.status,
					Body:       openAIErrorJSON("invalid_request_error", tc.code, promptUsagePolicyRejectionForTest),
				},
			}, nil)
			if finish.encryptedContentCached != tc.wantRemoval {
				t.Fatalf("encrypted content cached = %v, want %v", finish.encryptedContentCached, tc.wantRemoval)
			}
			retry := promptPolicyTestRequest("apikey", "opaque-first", "fresh", " changed visible prompt")
			retryBegin := hash.Begin(retry, http.MethodPost, "/v1/responses", "")
			if retryBegin.outcome != nil {
				t.Fatalf("changed request unexpectedly blocked: %+v", retryBegin)
			}
			session := preprocessPromptPolicyTestRequest(retryBegin, retry)
			if session.Sanitized() != tc.wantRemoval || gjson.GetBytes(retry.Body, "input.0.encrypted_content").Exists() == tc.wantRemoval {
				t.Fatalf("ciphertext removal must match explicit safety rejection: %s", retry.Body)
			}
			if got := gjson.GetBytes(retry.Body, "input.1.encrypted_content").String(); got != "fresh" {
				t.Fatalf("fresh encrypted_content = %q, want preserved", got)
			}
		})
	}
}

func TestInvalidEncryptedContentParamDoesNotCacheCiphertext(t *testing.T) {
	first := validGPTReasoningEncryptedContentForTestMarker(0x91)
	second := validGPTReasoningEncryptedContentForTestMarker(0x92)
	hash := &enabledTextHash{}
	req := promptPolicyTestRequest("apikey", first, second, " parameter-selected encrypted content")
	begin := hash.Begin(req, http.MethodPost, "/v1/responses", "")
	preprocessPromptPolicyTestRequest(begin, req)
	outcome := sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Body:       []byte(`{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified.","param":"input[1].encrypted_content"}}`),
		},
	}
	finish := begin.request.Finish(outcome, nil)
	if finish.encryptedContentCached {
		t.Fatalf("parameter-selected finish = %+v, want no ciphertext cached", finish)
	}
	scope := encryptedContentScopeHash(req)
	if hash.encryptedContent.contains(encryptedContentHashWithScope(scope, first), time.Now()) {
		t.Fatal("error.param cached the wrong encrypted_content")
	}
	if hash.encryptedContent.contains(encryptedContentHashWithScope(scope, second), time.Now()) {
		t.Fatal("error.param must not cache the selected encrypted_content")
	}
}

func TestIsPromptUsagePolicyRejectionText(t *testing.T) {
	if !isPromptUsagePolicyRejectionText(promptUsagePolicyRejectionForTest) {
		t.Fatal("exact prompt usage policy rejection was not recognized")
	}
	if isPromptUsagePolicyRejectionText("Invalid prompt. Please follow the usage policy.") {
		t.Fatal("generic invalid prompt must not activate the rejection cache")
	}
	success := sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK},
		Reason:   promptUsagePolicyRejectionForTest,
	}
	if isPromptUsagePolicyRejectionOutcome(success, nil) {
		t.Fatal("successful output must not be classified as a prompt policy rejection")
	}
	wrongCode := promptUsagePolicyFailureOutcomeForTest()
	wrongCode.Upstream.Body = []byte(`{"error":{"type":"invalid_request_error","code":"client","message":"` +
		promptUsagePolicyRejectionForTest + `"}}`)
	if isPromptUsagePolicyRejectionOutcome(wrongCode, nil) {
		t.Fatal("prompt policy message without upstream invalid_prompt code was classified as a rejection")
	}
}

func TestIsEncryptedContentSafetyRejectionOutcome(t *testing.T) {
	if !isEncryptedContentSafetyRejectionOutcome(promptUsagePolicyFailureOutcomeForTest(), nil) {
		t.Fatal("prompt usage policy rejection did not activate encrypted_content filtering")
	}
	if !isEncryptedContentSafetyRejectionOutcome(cybersecurityRiskFailureOutcomeForTest(), nil) {
		t.Fatal("cybersecurity risk rejection did not activate encrypted_content filtering")
	}
	wrongCyberCode := cybersecurityRiskFailureOutcomeForTest()
	wrongCyberCode.Upstream.Body = []byte(`{"error":{"type":"invalid_request_error","code":"client","message":"` +
		cybersecurityRiskMessage + `"}}`)
	if isEncryptedContentSafetyRejectionOutcome(wrongCyberCode, nil) {
		t.Fatal("cybersecurity message without cybersecurity_risk classification activated encrypted_content filtering")
	}
	otherClientError := sdk.ForwardOutcome{
		Kind:     sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusBadRequest},
		Reason:   "invalid request",
	}
	if isEncryptedContentSafetyRejectionOutcome(otherClientError, nil) {
		t.Fatal("unrelated client error activated encrypted_content filtering")
	}
}

func TestSafetyRejectionClassificationUsesUpstreamCodes(t *testing.T) {
	promptFailure := classifyResponsesError(
		"invalid_request_error",
		promptUsagePolicyErrorCode,
		promptUsagePolicyRejectionForTest,
	)
	if promptFailure.Code != promptUsagePolicyErrorCode || promptFailure.StatusCode != http.StatusBadRequest {
		t.Fatalf("prompt policy failure = %+v", promptFailure)
	}

	cyberFailure := classifyResponsesError(
		"invalid_request",
		"cyber_policy",
		cybersecurityRiskMessage,
	)
	if cyberFailure.Code != cybersecurityRiskErrorCode || cyberFailure.StatusCode != http.StatusBadRequest {
		t.Fatalf("cybersecurity failure = %+v", cyberFailure)
	}
}
