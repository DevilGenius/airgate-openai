package gateway

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zeebo/xxh3"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const (
	textSafetyRequestCacheTTL        = defaultSafetyRequestCacheTTL
	textSafetyRequestCacheMaxEntries = defaultSafetyRequestCacheMaxEntries
	requestRetryCacheTTL             = 24 * time.Hour
	requestRetryCacheMaxEntries      = 100_000
	textRequestHashDomain            = "airgate:text-request:xxh3-64:v1"
	encryptedContentHashDomain       = "airgate:encrypted-content-retry:xxh3-64:v1"
	cybersecurityRiskErrorCode       = "cybersecurity_risk"
	textSafetyCacheRetryAfter        = 10 * time.Minute
	textSafetyRateLimitCode          = "rate_limit_exceeded"
	textSafetyRateLimitMessage       = "Rate limit exceeded. Please retry later."
	cybersecurityRiskMessage         = "This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request. To get authorized for security work, join the Trusted Access for Cyber program: https://chatgpt.com/cyber"
)

type enabledTextHash struct {
	textSafety   safetyRequestCache
	requestRetry safetyRequestCache
}

type enabledTextHashRequest struct {
	hash                *enabledTextHash
	value               uint64
	ready               bool
	contextWindowCached bool
	dispatchClientModel string
	longContextModel    string
	encryptedContent    encryptedContentHashSession
}

type enabledEncryptedContentHashSession struct {
	cache          *safetyRequestCache
	checkedAt      time.Time
	validHashes    []uint64
	retrySanitized bool
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
	request.value = hash
	request.ready = true
	if h.textSafety.contains(hash, time.Now()) {
		outcome := textSafetyCacheHitOutcome(isAnthropicTextRequest(req, path))
		begin.outcome = &outcome
		begin.event = textHashBeginSafetyCacheHit
		return begin
	}

	request.dispatchClientModel = strings.TrimSpace(req.DispatchPlan.ClientModel)
	if request.dispatchClientModel == "" {
		request.dispatchClientModel = strings.TrimSpace(req.Model)
	}
	request.longContextModel = strings.TrimSpace(longContextModel)
	begin.dispatchClientModel = request.dispatchClientModel
	begin.longContextModel = request.longContextModel
	if request.longContextModel != "" {
		request.contextWindowCached = h.requestRetry.contains(hash, time.Now())
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
		request.encryptedContent = &enabledEncryptedContentHashSession{
			cache:     &h.requestRetry,
			checkedAt: time.Now(),
		}
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
	result.encryptedContentSanitized = encrypted.RetrySanitized()
	if isInvalidEncryptedContentOutcome(outcome, err) {
		result.encryptedContentCached = encrypted.CacheRejected()
	}
	if outcome.SafetyRejected && r.ready {
		r.hash.textSafety.add(r.value, time.Now())
		result.textSafetyCached = true
	}
	return result
}

func (r *enabledTextHashRequest) cacheContextWindowExceeded() bool {
	if r == nil || r.hash == nil || !r.ready || r.contextWindowCached || r.longContextModel == "" ||
		modelIDsEqual(r.dispatchClientModel, r.longContextModel) {
		return false
	}
	r.hash.requestRetry.addHashesWithLimits(
		[]uint64{r.value},
		time.Now(),
		requestRetryCacheTTL,
		requestRetryCacheMaxEntries,
	)
	return true
}

func (s *enabledEncryptedContentHashSession) BeginRewrite() {
	if s == nil {
		return
	}
	s.validHashes = s.validHashes[:0]
	s.retrySanitized = false
}

func (s *enabledEncryptedContentHashSession) ShouldRemove(raw string) bool {
	if s == nil {
		return false
	}
	hash := encryptedContentRetryHash(raw)
	if s.cache != nil && s.cache.contains(hash, s.checkedAt) {
		s.retrySanitized = true
		return true
	}
	s.validHashes = append(s.validHashes, hash)
	return false
}

func (s *enabledEncryptedContentHashSession) RetrySanitized() bool {
	return s != nil && s.retrySanitized
}

func (s *enabledEncryptedContentHashSession) CacheRejected() bool {
	if s == nil || s.cache == nil || len(s.validHashes) == 0 {
		return false
	}
	s.cache.addHashesWithLimits(
		s.validHashes,
		time.Now(),
		requestRetryCacheTTL,
		requestRetryCacheMaxEntries,
	)
	return true
}

func (h *enabledTextHash) stats(now time.Time) (
	textSize, textCapacity, requestRetrySize, requestRetryCapacity int,
) {
	if h == nil {
		return 0, textSafetyRequestCacheMaxEntries, 0, requestRetryCacheMaxEntries
	}
	textSize, textCapacity = h.textSafety.stats(now)
	requestRetrySize, requestRetryCapacity = h.requestRetry.statsWithCapacity(
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

func encryptedContentRetryHash(raw string) uint64 {
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, encryptedContentHashDomain)
	writeXXH3HashStringPart(&hasher, raw)
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
	if outcome.Kind == sdk.OutcomeSuccess && outcome.Upstream.StatusCode < 400 && err == nil {
		return false
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	return isEncryptedContentVerificationError(
		outcome.Reason,
		string(outcome.Upstream.Body),
		errText,
	)
}

func isCybersecurityRiskRejectionText(values ...string) bool {
	text := strings.ToLower(strings.Join(values, " "))
	if !strings.Contains(text, "cybersecurity risk") {
		return false
	}
	return strings.Contains(text, "flagged") ||
		strings.Contains(text, "trusted access for cyber") ||
		strings.Contains(text, "chatgpt.com/cyber")
}
