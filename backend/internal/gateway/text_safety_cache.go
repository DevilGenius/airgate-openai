package gateway

import (
	"context"
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
	textSafetyRequestHashDomain      = "airgate:text-safety-request:xxh3-64:v1"
	cybersecurityRiskErrorCode       = "cybersecurity_risk"
	textSafetyCacheRetryAfter        = 10 * time.Minute
	textSafetyRateLimitCode          = "rate_limit_exceeded"
	textSafetyRateLimitMessage       = "Rate limit exceeded. Please retry later."
	cybersecurityRiskMessage         = "This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request. To get authorized for security work, join the Trusted Access for Cyber program: https://chatgpt.com/cyber"
)

type textSafetyRequestContextKey struct{}

func textSafetyRequestHash(req *sdk.ForwardRequest, method, path string) (uint64, bool) {
	if req == nil || len(req.Body) == 0 || !isTextGenerationRequestPath(path) {
		return 0, false
	}
	contentType := strings.ToLower(strings.TrimSpace(req.Headers.Get("Content-Type")))
	if separator := strings.IndexByte(contentType, ';'); separator >= 0 {
		contentType = strings.TrimSpace(contentType[:separator])
	}
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, textSafetyRequestHashDomain)
	writeXXH3HashStringPart(&hasher, strings.ToUpper(strings.TrimSpace(method)))
	writeXXH3HashStringPart(&hasher, normalizeGatewayRequestPath(path))
	writeXXH3HashStringPart(&hasher, req.Model)
	writeXXH3HashStringPart(&hasher, strconv.FormatBool(req.Stream))
	writeXXH3HashStringPart(&hasher, contentType)
	writeXXH3HashUint64(&hasher, uint64(len(req.Body)))
	writeXXH3HashUint64(&hasher, xxh3.Hash(req.Body))
	return hasher.Sum64(), true
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

func withTextSafetyRequestHash(ctx context.Context, hash uint64) context.Context {
	return context.WithValue(ctx, textSafetyRequestContextKey{}, hash)
}

func textSafetyRequestHashFromContext(ctx context.Context) (uint64, bool) {
	if ctx == nil {
		return 0, false
	}
	hash, ok := ctx.Value(textSafetyRequestContextKey{}).(uint64)
	return hash, ok
}

func (g *OpenAIGateway) checkTextSafetyRequest(ctx context.Context, req *sdk.ForwardRequest, method, path string) (context.Context, *sdk.ForwardOutcome) {
	hash, ok := textSafetyRequestHash(req, method, path)
	if !ok {
		return ctx, nil
	}
	if g != nil && g.textSafety.contains(hash, time.Now()) {
		outcome := textSafetyCacheHitOutcome(isAnthropicTextRequest(req, path))
		return ctx, &outcome
	}
	return withTextSafetyRequestHash(ctx, hash), nil
}

func (g *OpenAIGateway) cacheTextSafetyRejection(ctx context.Context) {
	if g == nil {
		return
	}
	if hash, ok := textSafetyRequestHashFromContext(ctx); ok {
		g.textSafety.add(hash, time.Now())
	}
}

func textSafetyCacheHitOutcome(anthropic bool) sdk.ForwardOutcome {
	body := openAIErrorJSON("rate_limit_error", textSafetyRateLimitCode, textSafetyRateLimitMessage)
	if anthropic {
		body = anthropicErrorJSONWithCode("rate_limit_error", textSafetyRateLimitCode, textSafetyRateLimitMessage)
	}
	retryAfterSeconds := int(textSafetyCacheRetryAfter / time.Second)
	return sdk.ForwardOutcome{
		// 本地缓存命中不归罪于上游账号，也不触发新的安全缓存写入。
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

func isCybersecurityRiskRejectionText(values ...string) bool {
	text := strings.ToLower(strings.Join(values, " "))
	if !strings.Contains(text, "cybersecurity risk") {
		return false
	}
	return strings.Contains(text, "flagged") ||
		strings.Contains(text, "trusted access for cyber") ||
		strings.Contains(text, "chatgpt.com/cyber")
}
