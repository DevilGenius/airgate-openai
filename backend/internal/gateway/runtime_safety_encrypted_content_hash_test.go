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

func TestPromptUsagePolicyCachesWholeRequestAndAllEncryptedContent(t *testing.T) {
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
	if !finish.textSafetyCached || !finish.encryptedContentCached {
		t.Fatalf("finish = %+v, want request and encrypted content cached", finish)
	}
	if !hash.encryptedContent.contains(encryptedContentHash(first), time.Now()) {
		t.Fatal("first encrypted_content hash was not recorded")
	}
	if !hash.encryptedContent.contains(encryptedContentHash(second), time.Now()) {
		t.Fatal("second encrypted_content hash was not recorded")
	}

	retry := promptPolicyTestRequest("apikey", first, second, "")
	retryBegin := hash.Begin(retry, http.MethodPost, "/v1/responses", "")
	if retryBegin.outcome == nil || retryBegin.event != textHashBeginSafetyCacheHit {
		t.Fatalf("complete retry begin = %+v, want prompt policy cache hit", retryBegin)
	}
	if retryBegin.outcome.Upstream.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("complete retry status = %d, want 429", retryBegin.outcome.Upstream.StatusCode)
	}
}

func TestPromptUsagePolicyEncryptedContentMatchRemovesCachedCiphertext(t *testing.T) {
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
		t.Fatal("request reusing a rejected ciphertext was not marked sanitized")
	}
	if gjson.GetBytes(changedReq.Body, "input.0.encrypted_content").Exists() ||
		gjson.GetBytes(changedReq.Body, "input.0.id").Exists() {
		t.Fatalf("changed request retained cached encrypted reasoning data: %s", changedReq.Body)
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

func TestPromptUsagePolicyCacheDoesNotDependOnAccountType(t *testing.T) {
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
	if apiKeyExactRetryBegin.outcome == nil || apiKeyExactRetryBegin.event != textHashBeginSafetyCacheHit {
		t.Fatalf("API Key exact retry did not share the response-driven cache: %+v", apiKeyExactRetryBegin)
	}
	if apiKeyExactRetryBegin.outcome.Upstream.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("API Key exact retry status = %d, want 429", apiKeyExactRetryBegin.outcome.Upstream.StatusCode)
	}

	apiKeyChanged := promptPolicyTestRequest("apikey", rejected, fresh, " changed account type")
	apiKeyChangedBegin := hash.Begin(apiKeyChanged, http.MethodPost, "/v1/responses", "")
	if apiKeyChangedBegin.outcome != nil {
		t.Fatalf("changed API Key request unexpectedly hit full request cache: %+v", apiKeyChangedBegin.outcome)
	}
	apiKeyChangedSession := preprocessPromptPolicyTestRequest(apiKeyChangedBegin, apiKeyChanged)
	if !apiKeyChangedSession.Sanitized() {
		t.Fatal("API Key request reusing rejected ciphertext did not remove encrypted_content")
	}
	if gjson.GetBytes(apiKeyChanged.Body, "input.0.encrypted_content").Exists() {
		t.Fatalf("changed API Key request retained cached encrypted_content: %s", apiKeyChanged.Body)
	}
	if got := gjson.GetBytes(apiKeyChanged.Body, "input.1.encrypted_content").String(); got != fresh {
		t.Fatalf("changed API Key request removed fresh encrypted_content: got %q, want %q", got, fresh)
	}
}

func TestCybersecurityRiskRecordsAndRemovesEncryptedContent(t *testing.T) {
	rejectedFirst := validGPTReasoningEncryptedContentForTestMarker(0x61)
	rejectedSecond := validGPTReasoningEncryptedContentForTestMarker(0x62)
	fresh := validGPTReasoningEncryptedContentForTestMarker(0x63)
	hash := &enabledTextHash{}

	firstReq := promptPolicyTestRequest("apikey", rejectedFirst, rejectedSecond, " cybersecurity")
	firstBegin := hash.Begin(firstReq, http.MethodPost, "/v1/responses", "")
	preprocessPromptPolicyTestRequest(firstBegin, firstReq)
	finish := firstBegin.request.Finish(cybersecurityRiskFailureOutcomeForTest(), nil)
	if !finish.encryptedContentCached || !finish.textSafetyCached {
		t.Fatalf("cybersecurity finish = %+v, want ciphertext and full request cached", finish)
	}
	if !hash.encryptedContent.contains(encryptedContentHash(rejectedFirst), time.Now()) {
		t.Fatal("first cybersecurity encrypted_content hash was not recorded")
	}
	if !hash.encryptedContent.contains(encryptedContentHash(rejectedSecond), time.Now()) {
		t.Fatal("second cybersecurity encrypted_content hash was not recorded")
	}

	exactRetry := promptPolicyTestRequest("oauth", rejectedFirst, rejectedSecond, " cybersecurity")
	exactRetryBegin := hash.Begin(exactRetry, http.MethodPost, "/v1/responses", "")
	if exactRetryBegin.outcome == nil || exactRetryBegin.event != textHashBeginSafetyCacheHit {
		t.Fatalf("cybersecurity exact retry = %+v, want safety cache hit", exactRetryBegin)
	}
	if exactRetryBegin.outcome.Upstream.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("cybersecurity exact retry status = %d, want 429", exactRetryBegin.outcome.Upstream.StatusCode)
	}

	changedReq := promptPolicyTestRequest("oauth", rejectedFirst, fresh, " changed cybersecurity request")
	changedBegin := hash.Begin(changedReq, http.MethodPost, "/v1/responses", "")
	if changedBegin.outcome != nil {
		t.Fatalf("changed cybersecurity request unexpectedly hit full request cache: %+v", changedBegin.outcome)
	}
	changedSession := preprocessPromptPolicyTestRequest(changedBegin, changedReq)
	if !changedSession.Sanitized() {
		t.Fatal("cybersecurity ciphertext match did not activate unified removal")
	}
	if gjson.GetBytes(changedReq.Body, "input.0.encrypted_content").Exists() ||
		gjson.GetBytes(changedReq.Body, "input.0.id").Exists() {
		t.Fatalf("changed cybersecurity request retained cached encrypted reasoning data: %s", changedReq.Body)
	}
	if got := gjson.GetBytes(changedReq.Body, "input.1.encrypted_content").String(); got != fresh {
		t.Fatalf("changed cybersecurity request removed fresh encrypted_content: got %q, want %q", got, fresh)
	}
}

func TestEncryptedContentViolationTypesShareOneCache(t *testing.T) {
	rejected := validGPTReasoningEncryptedContentForTestMarker(0x71)
	secondRejected := validGPTReasoningEncryptedContentForTestMarker(0x72)
	fresh := validGPTReasoningEncryptedContentForTestMarker(0x73)
	hash := &enabledTextHash{}

	invalidReq := promptPolicyTestRequest("apikey", rejected, secondRejected, " invalid encrypted")
	invalidBegin := hash.Begin(invalidReq, http.MethodPost, "/v1/responses", "")
	preprocessPromptPolicyTestRequest(invalidBegin, invalidReq)
	invalidFinish := invalidBegin.request.Finish(invalidEncryptedContentFailureOutcomeForTest(rejected), nil)
	if !invalidFinish.encryptedContentCached {
		t.Fatal("invalid_encrypted_content did not populate the unified cache")
	}
	if size, _ := hash.encryptedContent.statsWithCapacity(time.Now(), encryptedContentCacheMaxEntries); size != 2 {
		t.Fatalf("unified encrypted_content cache size = %d, want all ciphertexts from the rejected request", size)
	}
	if !hash.encryptedContent.contains(encryptedContentHash(secondRejected), time.Now()) {
		t.Fatal("invalid_encrypted_content did not cache every ciphertext from the rejected request")
	}

	safetyReq := promptPolicyTestRequest("oauth", rejected, fresh, " shared cache")
	safetyBegin := hash.Begin(safetyReq, http.MethodPost, "/v1/responses", "")
	safetySession := preprocessPromptPolicyTestRequest(safetyBegin, safetyReq)
	if !safetySession.Sanitized() {
		t.Fatal("ciphertext cached by invalid_encrypted_content was not removed on the next request")
	}
	if gjson.GetBytes(safetyReq.Body, "input.0.encrypted_content").Exists() {
		t.Fatalf("shared cache retained rejected encrypted_content: %s", safetyReq.Body)
	}
	if got := gjson.GetBytes(safetyReq.Body, "input.1.encrypted_content").String(); got != fresh {
		t.Fatalf("shared cache removed fresh encrypted_content: got %q, want %q", got, fresh)
	}
	safetyBegin.request.Finish(promptUsagePolicyFailureOutcomeForTest(), nil)
	if size, _ := hash.encryptedContent.statsWithCapacity(time.Now(), encryptedContentCacheMaxEntries); size != 3 {
		t.Fatalf("unified encrypted_content cache size after safety rejection = %d, want 3", size)
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
