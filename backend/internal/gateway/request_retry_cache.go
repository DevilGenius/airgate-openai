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
	requestRetryCacheTTL        = 24 * time.Hour
	requestRetryCacheMaxEntries = 100_000
	encryptedContentHashDomain  = "airgate:encrypted-content-retry:xxh3-64:v1"
)

type contextWindowRerouteState struct {
	hash                uint64
	hashReady           bool
	cached              bool
	dispatchClientModel string
	longContextModel    string
}

func encryptedContentRetryHash(raw string) uint64 {
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, encryptedContentHashDomain)
	writeXXH3HashStringPart(&hasher, raw)
	return hasher.Sum64()
}

func (g *OpenAIGateway) effectiveLongContextModel() string {
	if g != nil && g.longContextModelID != "" {
		return g.longContextModelID
	}
	return configuredLongContextModel()
}

func (g *OpenAIGateway) checkContextWindowReroute(
	ctx context.Context,
	req *sdk.ForwardRequest,
	path string,
) (contextWindowRerouteState, *sdk.ForwardOutcome) {
	state := contextWindowRerouteState{}
	if g == nil || req == nil {
		return state, nil
	}
	hash, ok := textRequestHashFromContext(ctx)
	if !ok {
		return state, nil
	}
	state.hash = hash
	state.hashReady = true
	// DispatchPlan.ClientModel 是 Core 本次解析调度计划所使用的模型。不能用
	// UpstreamModel 判断重路由是否已经应用，因为 DSL 可能把配置的长上下文模型
	// 映射到名称完全不同的 scheduling/wire model。
	state.dispatchClientModel = strings.TrimSpace(req.DispatchPlan.ClientModel)
	if state.dispatchClientModel == "" {
		state.dispatchClientModel = strings.TrimSpace(req.Model)
	}
	state.longContextModel = strings.TrimSpace(g.effectiveLongContextModel())
	if state.longContextModel == "" {
		return state, nil
	}
	state.cached = g.requestRetry.contains(hash, time.Now())
	if !state.cached || modelIDsEqual(state.dispatchClientModel, state.longContextModel) {
		return state, nil
	}
	outcome := contextWindowRerouteOutcome(isAnthropicTextRequest(req, path), state.longContextModel)
	return state, &outcome
}

func (g *OpenAIGateway) cacheContextWindowExceeded(state contextWindowRerouteState) bool {
	if g == nil || !state.hashReady || state.cached || state.longContextModel == "" ||
		modelIDsEqual(state.dispatchClientModel, state.longContextModel) {
		return false
	}
	g.requestRetry.addHashesWithLimits(
		[]uint64{state.hash},
		time.Now(),
		requestRetryCacheTTL,
		requestRetryCacheMaxEntries,
	)
	return true
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
