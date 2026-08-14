package gateway

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/zeebo/xxh3"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const (
	textSafetyRequestCacheTTL        = defaultSafetyRequestCacheTTL
	textSafetyRequestCacheMaxEntries = defaultSafetyRequestCacheMaxEntries
	requestRetryCacheTTL             = 24 * time.Hour
	requestRetryCacheMaxEntries      = 100_000
	textRequestHashDomain            = "airgate:text-request:xxh3-64:v1"
	textSafetyRequestHashDomain      = "airgate:text-safety-request:xxh3-64:v1"
	textPromptHashDomain             = "airgate:text-prompt:xxh3-64:v1"
	textHashScopeDomain              = "airgate:text-scope:xxh3-64:v1"
	encryptedContentScopeHashDomain  = "airgate:encrypted-content-scope:xxh3-64:v1"
	cybersecurityRiskErrorCode       = "cybersecurity_risk"
	promptUsagePolicyErrorCode       = "invalid_prompt"
	promptUsagePolicyRejectionPhrase = "invalid prompt: your prompt was flagged as potentially violating our usage policy"
	textSafetyCacheRetryAfter        = 10 * time.Minute
	textSafetyRateLimitCode          = "rate_limit_exceeded"
	textSafetyRateLimitMessage       = "Rate limit exceeded. Please retry later."
	cybersecurityRiskMessage         = "This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request. To get authorized for security work, join the Trusted Access for Cyber program: https://chatgpt.com/cyber"
)

type enabledTextHash struct {
	textSafety       safetyRequestCache
	cyberSafety      safetyRequestCache
	promptSafety     safetyRequestCache
	requestRetry     safetyRequestCache
	encryptedContent safetyRequestCache
}

type enabledTextHashRequest struct {
	hash                *enabledTextHash
	value               uint64
	ready               bool
	contextValue        uint64
	promptValue         uint64
	promptReady         bool
	contextWindowCached bool
	dispatchClientModel string
	longContextModel    string
	encryptedContent    encryptedContentHashSession
}

func (h *enabledTextHash) Begin(
	req *sdk.ForwardRequest,
	method, path, longContextModel string,
) textHashBegin {
	request := &enabledTextHashRequest{
		hash:             h,
		encryptedContent: disabledEncryptedContent,
	}
	begin := textHashBegin{request: request}
	hash, ok := textRequestHash(req, method, path)
	if !ok {
		return begin
	}
	request.contextValue = hash
	request.value = textSafetyRequestHash(req, hash)
	request.ready = true
	if h.textSafety.contains(request.value, time.Now()) {
		outcome := textSafetyCacheHitOutcome(isAnthropicTextRequest(req, path))
		begin.outcome = &outcome
		begin.event = textHashBeginSafetyCacheHit
		return begin
	}
	if promptHash, promptOK := textPromptHash(req, method, path); promptOK {
		request.promptValue = promptHash
		request.promptReady = true
		if h.cyberSafety.contains(promptHash, time.Now()) {
			outcome := textSafetyCacheHitOutcome(isAnthropicTextRequest(req, path))
			begin.outcome = &outcome
			begin.event = textHashBeginCyberSafetyCacheHit
			return begin
		}
		if h.promptSafety.contains(promptHash, time.Now()) {
			outcome := textSafetyCacheHitOutcome(isAnthropicTextRequest(req, path))
			begin.outcome = &outcome
			begin.event = textHashBeginPromptSafetyCacheHit
			return begin
		}
	}
	request.dispatchClientModel = strings.TrimSpace(req.DispatchPlan.ClientModel)
	if request.dispatchClientModel == "" {
		request.dispatchClientModel = strings.TrimSpace(req.Model)
	}
	request.longContextModel = strings.TrimSpace(longContextModel)
	begin.dispatchClientModel = request.dispatchClientModel
	begin.longContextModel = request.longContextModel
	if request.longContextModel != "" {
		request.contextWindowCached = h.requestRetry.contains(request.contextValue, time.Now())
		if request.contextWindowCached && !modelIDsEqual(request.dispatchClientModel, request.longContextModel) {
			outcome := contextWindowRerouteOutcome(
				isAnthropicTextRequest(req, path),
				request.longContextModel,
			)
			begin.outcome = &outcome
			begin.event = textHashBeginContextWindowReroute
			return begin
		}
	}

	if isResponsesRequestPath(path) && bytes.Contains(req.Body, []byte(`"encrypted_content"`)) {
		request.encryptedContent = newEncryptedContentHashSession(
			&h.encryptedContent,
			encryptedContentScopeHash(req),
		)
	}
	return begin
}

func (r *enabledTextHashRequest) EncryptedContent() encryptedContentHashSession {
	if r == nil || r.encryptedContent == nil {
		return disabledEncryptedContent
	}
	return r.encryptedContent
}

func (r *enabledTextHashRequest) Finish(outcome sdk.ForwardOutcome, err error) textHashFinish {
	result := textHashFinish{}
	if r == nil || r.hash == nil {
		return result
	}
	result.dispatchClientModel = r.dispatchClientModel
	result.longContextModel = r.longContextModel
	if isContextWindowExceededForwardResult(outcome, err) {
		if r.contextWindowCached {
			result.contextWindowLongModelFailed = true
		} else if r.cacheContextWindowExceeded() {
			result.contextWindowCached = true
		}
	}

	encrypted := r.EncryptedContent()
	result.encryptedContentSanitized = encrypted.Sanitized()
	rejection, rejected := explicitTextHashRejectionFromOutcome(outcome, err)
	if rejected {
		now := time.Now()
		switch rejection.Code {
		case "invalid_encrypted_content":
			result.encryptedContentCached = encrypted.CacheViolation(rejection)
		case promptUsagePolicyErrorCode:
			if r.ready {
				r.hash.textSafety.addWithCategory(r.value, now, promptUsagePolicyErrorCode)
				result.textSafetyCached = true
			}
			if r.promptReady {
				r.hash.promptSafety.add(r.promptValue, now)
				result.promptSafetyCached = true
			}
		case cybersecurityRiskErrorCode:
			if r.ready {
				r.hash.textSafety.addWithCategory(r.value, now, cybersecurityRiskErrorCode)
				result.textSafetyCached = true
			}
			if r.promptReady {
				r.hash.cyberSafety.add(r.promptValue, now)
				result.cyberSafetyCached = true
			}
		}
	}
	return result
}

func (r *enabledTextHashRequest) cacheContextWindowExceeded() bool {
	if r == nil || r.hash == nil || !r.ready || r.contextWindowCached || r.longContextModel == "" ||
		modelIDsEqual(r.dispatchClientModel, r.longContextModel) {
		return false
	}
	r.hash.requestRetry.addHashesWithLimits(
		[]uint64{r.contextValue},
		time.Now(),
		requestRetryCacheTTL,
		requestRetryCacheMaxEntries,
	)
	return true
}

func (h *enabledTextHash) stats(now time.Time) (
	textSize, textCapacity,
	textCybersecurityRisk, textInvalidPrompt,
	cyberSize, cyberCapacity,
	promptSize, promptCapacity,
	encryptedContentSize, encryptedContentCapacity,
	contextWindowSize, contextWindowCapacity int,
) {
	if h == nil {
		return 0, textSafetyRequestCacheMaxEntries,
			0, 0,
			0, textSafetyRequestCacheMaxEntries,
			0, textSafetyRequestCacheMaxEntries,
			0, encryptedContentCacheMaxEntries,
			0, requestRetryCacheMaxEntries
	}
	textSize, textCapacity, textCategoryCounts := h.textSafety.statsWithCategoryCounts(
		now,
		textSafetyRequestCacheMaxEntries,
		cybersecurityRiskErrorCode,
		promptUsagePolicyErrorCode,
	)
	textCybersecurityRisk = textCategoryCounts[0]
	textInvalidPrompt = textCategoryCounts[1]
	cyberSize, cyberCapacity = h.cyberSafety.stats(now)
	promptSize, promptCapacity = h.promptSafety.stats(now)
	encryptedContentSize, encryptedContentCapacity = h.encryptedContent.statsWithCapacity(
		now,
		encryptedContentCacheMaxEntries,
	)
	contextWindowSize, contextWindowCapacity = h.requestRetry.statsWithCapacity(
		now,
		requestRetryCacheMaxEntries,
	)
	return
}

func textRequestHash(req *sdk.ForwardRequest, method, path string) (uint64, bool) {
	if req == nil || len(req.Body) == 0 || !isTextGenerationRequestPath(path) {
		return 0, false
	}
	contentType := strings.ToLower(strings.TrimSpace(req.Headers.Get("Content-Type")))
	if separator := strings.IndexByte(contentType, ';'); separator >= 0 {
		contentType = strings.TrimSpace(contentType[:separator])
	}
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, textRequestHashDomain)
	writeXXH3HashStringPart(&hasher, strings.ToUpper(strings.TrimSpace(method)))
	writeXXH3HashStringPart(&hasher, normalizeGatewayRequestPath(path))
	writeXXH3HashStringPart(&hasher, req.Model)
	writeXXH3HashStringPart(&hasher, strconv.FormatBool(req.Stream))
	writeXXH3HashStringPart(&hasher, contentType)
	writeXXH3HashUint64(&hasher, uint64(len(req.Body)))
	writeXXH3HashUint64(&hasher, xxh3.Hash(req.Body))
	return hasher.Sum64(), true
}

func textSafetyRequestHash(req *sdk.ForwardRequest, requestHash uint64) uint64 {
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, textSafetyRequestHashDomain)
	writeXXH3HashUint64(&hasher, textHashScope(req))
	writeXXH3HashUint64(&hasher, requestHash)
	return hasher.Sum64()
}

func textPromptHash(req *sdk.ForwardRequest, method, path string) (uint64, bool) {
	if req == nil || len(req.Body) == 0 || !isTextGenerationRequestPath(path) {
		return 0, false
	}

	normalizedPath := normalizeGatewayRequestPath(path)
	body := req.Body
	if isResponsesRequestPath(normalizedPath) && bytes.Contains(body, []byte(`"encrypted_content"`)) {
		if updated, changed := rewriteResponsesReasoningEncryptedContentKnownPresent(body, reasoningEncryptedContentRewritePolicy{
			removeInvalid:          true,
			stripExistingOrphanIDs: true,
			stripRemovedContentID:  true,
			removeValid: func(string) bool {
				return true
			},
		}); changed {
			body = updated
		}
	}

	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return 0, false
	}
	fields := []string(nil)
	switch normalizedPath {
	case "/v1/responses", "/responses":
		fields = []string{"instructions", "input", "tools", "prompt", "previous_response_id", "conversation"}
	case "/v1/chat/completions", "/chat/completions":
		fields = []string{"messages", "tools", "functions"}
	case "/v1/messages", "/messages":
		fields = []string{"system", "messages", "tools"}
	default:
		return 0, false
	}

	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, textPromptHashDomain)
	writeXXH3HashUint64(&hasher, textHashScope(req))
	writeXXH3HashStringPart(&hasher, strings.ToUpper(strings.TrimSpace(method)))
	writeXXH3HashStringPart(&hasher, normalizedPath)
	writeXXH3HashStringPart(&hasher, req.Model)
	written := false
	for _, field := range fields {
		value := root.Get(field)
		if !value.Exists() {
			continue
		}
		writeXXH3HashStringPart(&hasher, field)
		writeXXH3HashStringPart(&hasher, value.Raw)
		written = true
	}
	if !written {
		return 0, false
	}
	return hasher.Sum64(), true
}

func textHashScope(req *sdk.ForwardRequest) uint64 {
	if req == nil {
		return 0
	}
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, textHashScopeDomain)
	if req.Headers != nil {
		for _, name := range []string{
			"X-Airgate-User-ID",
			"X-Airgate-API-Key-ID",
			"X-Airgate-Group-ID",
		} {
			writeXXH3HashStringPart(&hasher, strings.TrimSpace(req.Headers.Get(name)))
		}
	}
	if req.Account != nil {
		writeXXH3HashUint64(&hasher, uint64(req.Account.ID))
		writeXXH3HashStringPart(&hasher, strings.TrimSpace(req.Account.Type))
	}
	return hasher.Sum64()
}

func encryptedContentScopeHash(req *sdk.ForwardRequest) uint64 {
	if req == nil {
		return 0
	}
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, encryptedContentScopeHashDomain)
	if req.Account != nil {
		writeXXH3HashUint64(&hasher, uint64(req.Account.ID))
		writeXXH3HashStringPart(&hasher, strings.TrimSpace(req.Account.Type))
	}
	writeXXH3HashStringPart(&hasher, strings.TrimSpace(req.Model))
	if req.Headers != nil {
		for _, name := range []string{
			"session_id",
			"conversation_id",
			"x-codex-turn-state",
			"X-Airgate-User-ID",
			"X-Airgate-API-Key-ID",
		} {
			writeXXH3HashStringPart(&hasher, strings.TrimSpace(req.Headers.Get(name)))
		}
	}
	for _, path := range []string{"safety_identifier", "prompt_cache_key", "metadata.user_id"} {
		writeXXH3HashStringPart(&hasher, strings.TrimSpace(gjson.GetBytes(req.Body, path).String()))
	}
	return hasher.Sum64()
}

func isTextGenerationRequestPath(path string) bool {
	switch normalizeGatewayRequestPath(path) {
	case "/v1/responses", "/responses",
		"/v1/chat/completions", "/chat/completions",
		"/v1/messages", "/messages":
		return true
	default:
		return false
	}
}

func isAnthropicTextRequest(req *sdk.ForwardRequest, path string) bool {
	if req != nil && isAnthropicRequest(req) {
		return true
	}
	switch normalizeGatewayRequestPath(path) {
	case "/v1/messages", "/messages":
		return true
	default:
		return false
	}
}

func effectiveLongContextModel() string {
	return configuredLongContextModel()
}

func textSafetyCacheHitOutcome(anthropic bool) sdk.ForwardOutcome {
	body := openAIErrorJSON("rate_limit_error", textSafetyRateLimitCode, textSafetyRateLimitMessage)
	if anthropic {
		body = anthropicErrorJSONWithCode("rate_limit_error", textSafetyRateLimitCode, textSafetyRateLimitMessage)
	}
	retryAfterSeconds := int(textSafetyCacheRetryAfter / time.Second)
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusTooManyRequests,
			Headers: http.Header{
				"Content-Type":   []string{"application/json"},
				"Content-Length": []string{strconv.Itoa(len(body))},
				"Retry-After":    []string{strconv.Itoa(retryAfterSeconds)},
			},
			Body: body,
		},
		Reason:     textSafetyRateLimitMessage,
		RetryAfter: textSafetyCacheRetryAfter,
	}
}

func contextWindowRerouteOutcome(anthropic bool, longContextModel string) sdk.ForwardOutcome {
	body := openAIErrorJSON("invalid_request_error", "context_too_large", contextTooLargeMessage)
	if anthropic {
		body = anthropicErrorJSONWithCode("invalid_request_error", "context_too_large", contextTooLargeMessage)
	}
	return sdk.ForwardOutcome{
		Kind:               sdk.OutcomeClientError,
		FailoverScope:      sdk.FailoverScopeModelReroute,
		RerouteClientModel: longContextModel,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Headers: http.Header{
				"Content-Type":   []string{"application/json"},
				"Content-Length": []string{strconv.Itoa(len(body))},
			},
			Body: body,
		},
		Reason: contextTooLargeMessage,
	}
}

func isContextWindowExceededForwardResult(outcome sdk.ForwardOutcome, err error) bool {
	return outcomeIsContextTooLarge(outcome) || isContextTooLargeErrorResult(err)
}

func isInvalidEncryptedContentOutcome(outcome sdk.ForwardOutcome, err error) bool {
	rejection, ok := explicitTextHashRejectionFromOutcome(outcome, err)
	return ok && rejection.Code == "invalid_encrypted_content"
}

func isPromptUsagePolicyRejectionOutcome(outcome sdk.ForwardOutcome, err error) bool {
	rejection, ok := explicitTextHashRejectionFromOutcome(outcome, err)
	return ok && rejection.Code == promptUsagePolicyErrorCode
}

func isCybersecurityRiskRejectionOutcome(outcome sdk.ForwardOutcome, err error) bool {
	rejection, ok := explicitTextHashRejectionFromOutcome(outcome, err)
	return ok && rejection.Code == cybersecurityRiskErrorCode
}

func explicitTextHashRejectionFromOutcome(outcome sdk.ForwardOutcome, err error) (explicitUpstreamError, bool) {
	if outcome.Kind == sdk.OutcomeSuccess && outcome.Upstream.StatusCode < http.StatusBadRequest && err == nil {
		return explicitUpstreamError{}, false
	}
	if rejection, ok := parseExplicitUpstreamError(outcome.Upstream.Body); ok &&
		isExplicitTextHashRejectionCode(rejection.Code) {
		return rejection, true
	}
	var failure *responsesFailureError
	if errors.As(err, &failure) && failure != nil && isExplicitTextHashRejectionCode(failure.Code) {
		code := normalizeExplicitUpstreamErrorCode(failure.Code)
		return explicitUpstreamError{
			UpstreamCode: strings.ToLower(strings.TrimSpace(failure.Code)),
			Code:         code,
			Message:      failure.upstreamReason(),
		}, true
	}
	return explicitUpstreamError{}, false
}

func isEncryptedContentSafetyRejectionOutcome(outcome sdk.ForwardOutcome, err error) bool {
	return isPromptUsagePolicyRejectionOutcome(outcome, err) ||
		isCybersecurityRiskRejectionOutcome(outcome, err)
}

func isPromptUsagePolicyRejectionText(values ...string) bool {
	text := strings.ToLower(strings.Join(values, " "))
	return strings.Contains(text, promptUsagePolicyRejectionPhrase)
}

func isCybersecurityRiskRejectionText(values ...string) bool {
	text := strings.ToLower(strings.Join(values, " "))
	return strings.Contains(text, strings.ToLower(cybersecurityRiskMessage))
}
