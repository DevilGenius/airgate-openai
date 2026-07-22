package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// ──────────────────────────────────────────────────────
// Anthropic Messages API 转发入口（纯 gjson/sjson，零 struct）
// ──────────────────────────────────────────────────────

// forwardAnthropicMessage 处理 Anthropic Messages API 请求
// 流程：原始 JSON → 验证 → 使用 Core 选中的 DispatchPlan → 一步直转 Responses API → 转发上游
func (g *OpenAIGateway) forwardAnthropicMessage(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	start := time.Now()
	body := req.Body
	strategy := resolveAnthropicUpstreamStrategy(req.Account)
	session := resolveOpenAISession(req.Headers, req.Body, req.Account.ID)
	session.DigestChain = buildAnthropicDigestChain(body)
	if session.SessionKey == "" {
		if reusedSessionID, matchedChain, ok := findAnthropicDigestSession(req.Account.ID, session.DigestChain); ok {
			session.SessionID = reusedSessionID
			session.SessionKey = sessionStateKeyFromValues(reusedSessionID, "", "")
			session.MatchedDigest = matchedChain
			session.FromStoredState = true
			session.SessionSource = "anthropic_digest_match"
		} else if session.DigestChain != "" {
			session.SessionID = deterministicUUIDFromSeed(fmt.Sprintf("anthropic:%d:%d:%s", req.Account.ID, time.Now().UnixNano(), session.DigestChain))
			session.SessionKey = sessionStateKeyFromValues(session.SessionID, "", "")
			session.SessionSource = "anthropic_digest_new"
		}
	}
	updateSessionStateFromRequest(session, req.Account.ID)

	logger := sdk.LoggerFromContext(ctx)
	logger.Debug("anthropic_request_received",
		sdk.LogFieldModel, gjson.GetBytes(body, "model").String(),
		"messages", gjson.GetBytes(body, "messages.#").Int(),
		"tools", gjson.GetBytes(body, "tools.#").Int(),
		"stream", gjson.GetBytes(body, "stream").Bool(),
		"last_msg", truncate(gjson.GetBytes(body, "messages.@last.content").String(), 200),
	)

	// 1. 验证请求（纯 gjson）
	if statusCode, errType, errMsg := validateAnthropicRequestJSON(body); statusCode != 0 {
		errBody := anthropicErrorJSON(errType, errMsg)
		// 客户端请求本身无效（缺字段 / 格式错）
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: statusCode,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       errBody,
			},
			Reason:   errMsg,
			Duration: time.Since(start),
		}, nil
	}

	// 2. 同步 stream（model 同步已在 forwardHTTP 入口的 preprocessRequestBody 统一处理）
	if req.Stream && !gjson.GetBytes(body, "stream").Bool() {
		body, _ = sjson.SetBytes(body, "stream", true)
	}

	// 3. 使用 Core 选中的上游模型；Claude -> OpenAI 的映射由 DispatchDSL 在 Core 内执行。
	originalModel := gjson.GetBytes(body, "model").String()
	var mappingEffort string
	if mapping := resolveAnthropicModelMapping(originalModel); mapping != nil {
		mappingEffort = mapping.ReasoningEffort
	}
	modelName := strings.TrimSpace(req.DispatchPlan.UpstreamModel())
	if modelName == "" {
		body := openAIErrorJSON("invalid_request_error", "invalid_request", "dispatch_plan is required")
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       body,
			},
			Reason:   "dispatch_plan is required",
			Duration: time.Since(start),
		}, nil
	}

	// 4. 一步直转为 Responses API JSON
	// 注: Anthropic 的 cache_control 在转换为 Responses API 后不再适用，
	// Responses API 使用 session_id + include 机制实现缓存，无需预处理断点
	compactSummaryRequest := isAnthropicCompactSummaryRequest(gjson.ParseBytes(body))
	body, _ = sjson.SetBytes(body, "model", modelName)
	fullResponsesBody := convertAnthropicRequestToResponses(body, modelName, mappingEffort)
	responsesBody := fullResponsesBody
	requestMode := "full_replay"
	requestReason := "no_session_anchor"
	if session.SessionKey != "" {
		requestReason = "session_anchor_present"
	}
	if strategy.allowsContinuation() {
		if continuationBody, ok := convertAnthropicRequestToResponsesContinuation(body, modelName, mappingEffort, session.PreviousRespID); ok {
			responsesBody = continuationBody
			requestMode = "continuation"
			requestReason = "previous_response_id_available"
		} else if session.PreviousRespID != "" {
			requestReason = "previous_response_id_unusable"
		}
	} else if session.PreviousRespID != "" {
		requestReason = "continuation_disabled"
	}
	// 5. 按需注入 web_search 工具
	explicitServiceTier := explicitAnthropicRequestServiceTier(req)
	responsesBody = finalizeAnthropicResponsesBody(responsesBody, body, explicitServiceTier)
	fullResponsesBody = finalizeAnthropicResponsesBody(fullResponsesBody, body, explicitServiceTier)
	responsesBody = injectAnthropicPromptCacheKey(responsesBody, strategy, session)
	fullResponsesBody = injectAnthropicPromptCacheKey(fullResponsesBody, strategy, session)

	logger.Debug("anthropic_request_converted",
		"model", gjson.GetBytes(responsesBody, "model").String(),
		"tools", gjson.GetBytes(responsesBody, "tools.#").Int(),
		"input_items", gjson.GetBytes(responsesBody, "input.#").Int(),
		"reasoning_effort", gjson.GetBytes(responsesBody, "reasoning.effort").String(),
		"client_effort_raw", gjson.GetBytes(body, "output_config.effort").String(),
		"client_thinking_type", gjson.GetBytes(body, "thinking.type").String(),
		"client_thinking_budget", gjson.GetBytes(body, "thinking.budget_tokens").Int(),
		"client_max_tokens", gjson.GetBytes(body, "max_tokens").Int(),
		"compact_summary", compactSummaryRequest,
		"verbosity", gjson.GetBytes(responsesBody, "text.verbosity").String(),
		"dispatch_rule_id", req.DispatchPlan.RuleID,
		"request_mode", requestMode,
		"request_reason", requestReason,
		"session_key", session.SessionKey,
		"session_present", session.SessionID != "" || session.ConversationID != "" || session.PromptCacheKey != "",
		"session_source", session.SessionSource,
		"previous_response_id", session.PreviousRespID,
		"prompt_cache_key", session.PromptCacheKey,
		"request_body_bytes", len(body),
		"responses_body_bytes", len(responsesBody),
		"messages_hash", shortHashBytes([]byte(gjson.GetBytes(body, "messages").Raw)),
		"system_hash", shortHashBytes([]byte(gjson.GetBytes(body, "system").Raw)),
		"responses_input_hash", shortHashBytes([]byte(gjson.GetBytes(responsesBody, "input").Raw)),
		"tool_choice_hash", shortHashBytes([]byte(gjson.GetBytes(responsesBody, "tool_choice").Raw)),
		"tools_hash", shortHashBytes([]byte(gjson.GetBytes(responsesBody, "tools").Raw)),
		"body_has_prompt_cache_key", gjson.GetBytes(responsesBody, "prompt_cache_key").Exists(),
		"digest_chain", session.DigestChain,
		"digest_matched", session.MatchedDigest,
	)

	// 6. 转发上游。模型降级由 Core 在下一次 failover 中选择下一个 DispatchPlan。
	replayBody := []byte(nil)
	if requestMode == "continuation" {
		replayBody = fullResponsesBody
	}

	outcome, err := g.doAnthropicForward(ctx, req, responsesBody, replayBody, originalModel, modelName, start, session)
	return outcome, err
}

// doAnthropicForward 执行 Anthropic 转发。
// mappedModel: 映射后的 GPT 模型名，用于 Core 计费（写入 Usage.Model）。
func (g *OpenAIGateway) doAnthropicForward(
	ctx context.Context,
	req *sdk.ForwardRequest,
	responsesBody []byte,
	replayBody []byte,
	originalModel string,
	mappedModel string,
	start time.Time,
	session openAISessionResolution,
) (sdk.ForwardOutcome, error) {
	outcome, _, err := g.forwardAnthropicResponses(ctx, req, responsesBody, originalModel, mappedModel, start, req.Writer, false, session)
	if err != nil {
		if len(replayBody) > 0 {
			var failure *responsesFailureError
			if errors.As(err, &failure) && failure.isContinuationAnchorError() && canReplayContinuationAnchor(err) {
				sdk.LoggerFromContext(ctx).Warn("anthropic_continuation_anchor_invalid",
					"session", session.SessionKey,
					"digest_chain", session.DigestChain)
				clearSessionStateResponseID(session.SessionKey)
				outcome, _, err = g.forwardAnthropicResponses(ctx, req, replayBody, originalModel, mappedModel, start, req.Writer, false, session)
			}
		}
		return outcome, err
	}
	return outcome, nil
}

func finalizeAnthropicResponsesBody(responsesBody []byte, originalBody []byte, serviceTier string) []byte {
	result := responsesBody
	if tier := normalizeOpenAIWireServiceTier(serviceTier); tier != "" {
		result, _ = sjson.SetBytes(result, "service_tier", tier)
	}
	if hasWebSearchTool(originalBody) {
		result = injectWebSearchToolJSON(result)
	}
	if !gjson.GetBytes(result, "tools").IsArray() || gjson.GetBytes(result, "tools.#").Int() == 0 {
		result, _ = sjson.DeleteBytes(result, "tools")
		result, _ = sjson.DeleteBytes(result, "tool_choice")
		result, _ = sjson.DeleteBytes(result, "parallel_tool_calls")
	}
	return normalizeResponsesToolCompatibility(result)
}

func explicitAnthropicRequestServiceTier(req *sdk.ForwardRequest) string {
	if req == nil {
		return ""
	}
	headerTier := ""
	if req.Headers != nil {
		headerTier = req.Headers.Get("X-Airgate-Service-Tier")
	}
	return firstNonEmptyTier(headerTier, gjson.GetBytes(req.Body, "service_tier").String())
}

func defaultAnthropicUsageServiceTier(_ *sdk.ForwardRequest) string {
	return ""
}

func injectAnthropicPromptCacheKey(responsesBody []byte, strategy anthropicUpstreamStrategy, session openAISessionResolution) []byte {
	if strategy == anthropicStrategyOAuth {
		return normalizePromptCacheKeyForUpstream(responsesBody)
	}
	if strings.TrimSpace(session.PromptCacheKey) == "" {
		return normalizePromptCacheKeyForUpstream(responsesBody)
	}
	if gjson.GetBytes(responsesBody, "prompt_cache_key").Exists() {
		return normalizePromptCacheKeyForUpstream(responsesBody)
	}
	next, err := sjson.SetBytes(responsesBody, "prompt_cache_key", upstreamPromptCacheKey(session.PromptCacheKey))
	if err != nil {
		return responsesBody
	}
	return next
}

// ──────────────────────────────────────────────────────
// 统一的 Anthropic → Responses API 转发（合并 OAuth/APIKey 路径）
// ──────────────────────────────────────────────────────

// forwardAnthropicResponses 统一的 Anthropic 转发。
// suppressErrorWrite: true 时上游错误不写入客户端，通过 errBody 返回供 fallback 判断。
// 返回值: outcome, errBody（仅 suppress 时非 nil）, error
func (g *OpenAIGateway) forwardAnthropicResponses(
	ctx context.Context,
	req *sdk.ForwardRequest,
	responsesBody []byte,
	originalModel string,
	mappedModel string,
	start time.Time,
	w http.ResponseWriter,
	suppressErrorWrite bool,
	session openAISessionResolution,
) (sdk.ForwardOutcome, []byte, error) {
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)
	responsesBody = applyForceInstructions(responsesBody, req.Headers)

	upstreamReq, err := g.buildAnthropicUpstreamRequest(ctx, req, account, responsesBody, session)
	if err != nil {
		reason := fmt.Sprintf("构建上游请求失败: %v", err)
		logger.Warn("upstream_request_build_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, mappedModel,
			"protocol", "anthropic",
			sdk.LogFieldError, err,
		)
		return transientOutcome(reason), nil, fmt.Errorf("%s", reason)
	}

	isOAuth := isOpenAIOAuthCredentials(account.Credentials)
	accountType := "apikey"
	if isOAuth {
		accountType = "oauth"
	}
	logger.Debug("upstream_request_start",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, mappedModel,
		"original_model", originalModel,
		"url", redactURL(upstreamReq.URL.String()),
		sdk.LogFieldMethod, http.MethodPost,
		"stream", gjson.GetBytes(req.Body, "stream").Bool(),
		"account_type", accountType,
		"protocol", "anthropic",
	)

	streamable := gjson.GetBytes(req.Body, "stream").Bool() && w != nil
	client := g.buildForwardHTTPClient(ctx, req, account)
	resp, cancel, err := g.doStreamableUpstream(ctx, client, upstreamReq, streamable)
	if err != nil {
		dur := time.Since(start)
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, mappedModel,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldError, err,
			"protocol", "anthropic",
		)
		return transientOutcome(err.Error()), nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := readLimitedErrorBody(resp.Body)
		if isOpenAIAgentIdentityAccount(account) && isAgentIdentityTaskInvalidHTTPResponse(resp.StatusCode, body) {
			// A task can expire while an Anthropic request is in flight.  Re-register
			// once before exposing the authentication failure to the caller.
			g.invalidateAgentIdentityTask(account, upstreamReq.Header)
			oldCancel := cancel
			_ = resp.Body.Close()
			if oldCancel != nil {
				oldCancel()
			}
			cancel = nil
			retryAuth, authErr := g.buildOpenAIAuthHeaders(ctx, account, true)
			if authErr != nil {
				reason := fmt.Sprintf("Agent Identity task 恢复失败: %v", authErr)
				return accountDeadOutcome(reason), nil, fmt.Errorf("%s", reason)
			}
			retryReq, buildErr := g.buildAnthropicUpstreamRequest(ctx, req, account, responsesBody, session)
			if buildErr != nil {
				reason := fmt.Sprintf("构建 Agent Identity 重试请求失败: %v", buildErr)
				return transientOutcome(reason), nil, fmt.Errorf("%s", reason)
			}
			retryReq.Header.Set("Authorization", retryAuth.Get("Authorization"))
			retryResp, retryCancel, retryErr := g.doStreamableUpstream(ctx, client, retryReq, streamable)
			if retryErr != nil {
				reason := fmt.Sprintf("Agent Identity 重试请求失败: %v", retryErr)
				return transientOutcome(reason), nil, fmt.Errorf("%s", reason)
			}
			resp = retryResp
			cancel = retryCancel
			if resp.StatusCode >= 400 {
				body, _ = readLimitedErrorBody(resp.Body)
			}
		}
		if resp.StatusCode >= 400 {
			if req.TraceFinalError {
				captureFinalErrorUpstreamBody(ctx, body)
			}
			dur := time.Since(start)
			logger.Warn("upstream_request_non_2xx",
				sdk.LogFieldAccountID, account.ID,
				sdk.LogFieldModel, mappedModel,
				sdk.LogFieldStatus, resp.StatusCode,
				sdk.LogFieldDurationMs, dur.Milliseconds(),
				"body_preview", truncate(string(body), 300),
				"protocol", "anthropic",
			)

			if suppressErrorWrite {
				// fallback 模式：不写客户端，把 body 返给调用方判断是否降级
				msg := extractOpenAIErrorMessage(body)
				if msg == "" {
					msg = truncate(string(body), 200)
				}
				outcome := failureOutcome(resp.StatusCode, body, resp.Header.Clone(), msg, extractRetryAfterHeader(resp.Header))
				outcome.Duration = time.Since(start)
				return outcome, body, nil
			}
			outcome, err := g.writeAnthropicUpstreamError(w, resp.StatusCode, body, start)
			return outcome, nil, err
		}
	}

	if snapshot := parseCodexUsageFromHeaders(resp.Header); snapshot != nil {
		StoreCodexUsage(account.ID, snapshot)
	}

	logger.Debug("upstream_request_completed",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, mappedModel,
		sdk.LogFieldStatus, resp.StatusCode,
		sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
		"content_length", resp.ContentLength,
		"stream", gjson.GetBytes(req.Body, "stream").Bool(),
		"protocol", "anthropic",
	)

	requestServiceTier := explicitAnthropicRequestServiceTier(req)
	defaultServiceTier := defaultAnthropicUsageServiceTier(req)
	resolvedEffort := gjson.GetBytes(responsesBody, "reasoning.effort").String()
	isStream := gjson.GetBytes(req.Body, "stream").Bool()
	if isStream && w != nil {
		if turnState := decodeTurnStateHeader(resp.Header); turnState != "" {
			updateSessionStateTurnState(session.SessionKey, turnState)
		}
		outcome, err := translateResponsesSSEToAnthropicSSE(ctx, resp, w, originalModel, mappedModel, req.Body, requestServiceTier, defaultServiceTier, start, g.streamIdleTimeout(), session)
		setUsageReasoningEffort(outcome.Usage, resolvedEffort)
		return outcome, nil, err
	}

	if turnState := decodeTurnStateHeader(resp.Header); turnState != "" {
		updateSessionStateTurnState(session.SessionKey, turnState)
	}
	nonStreamWriter := w
	if suppressErrorWrite {
		nonStreamWriter = nil
	}
	outcome, err := g.handleAnthropicNonStreamFromResponses(resp, nonStreamWriter, originalModel, mappedModel, req.Body, requestServiceTier, defaultServiceTier, start, session, req.Account.ID)
	setUsageReasoningEffort(outcome.Usage, resolvedEffort)
	if suppressErrorWrite && outcome.Kind == sdk.OutcomeClientError && outcome.Upstream.StatusCode >= 400 {
		errBody := outcome.Upstream.Body
		if len(errBody) == 0 {
			errBody = []byte(outcome.Reason)
		}
		return outcome, errBody, err
	}
	return outcome, nil, err
}

// buildAnthropicUpstreamRequest 构建 Anthropic 转发的上游 HTTP 请求
// 根据 OAuth/APIKey 设置不同的 URL、认证头和特殊处理
func (g *OpenAIGateway) buildAnthropicUpstreamRequest(
	ctx context.Context,
	req *sdk.ForwardRequest,
	account *sdk.Account,
	responsesBody []byte,
	session openAISessionResolution,
) (*http.Request, error) {
	isOAuth := isOpenAIOAuthCredentials(account.Credentials)
	responsesBody = applyOpenAIWireReasoningEffort(
		responsesBody,
		gjson.GetBytes(responsesBody, "model").String(),
	)

	// 确定目标 URL
	var targetURL string
	if isOAuth {
		targetURL = ChatGPTSSEURL
	} else {
		targetURL = buildAPIKeyURL(account, "/v1/responses")
	}

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(responsesBody))
	if err != nil {
		return nil, err
	}

	// 公共头
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "text/event-stream")

	if isOAuth {
		// OAuth 模式：Agent Identity 使用逐请求签名，普通 OAuth 使用 Bearer。
		authHeaders, authErr := g.buildOpenAIAuthHeaders(ctx, account, false)
		if authErr != nil {
			return nil, authErr
		}
		for key, values := range authHeaders {
			for _, value := range values {
				upstreamReq.Header.Add(key, value)
			}
		}
		if aid := account.Credentials["chatgpt_account_id"]; aid != "" {
			upstreamReq.Header.Set("ChatGPT-Account-ID", aid)
		}
		if session.SessionID != "" {
			upstreamReq.Header.Set("session_id", isolateSessionID(session.SessionID))
		}
		if session.ConversationID != "" {
			upstreamReq.Header.Set("conversation_id", isolateSessionID(session.ConversationID))
		}
		if session.LastTurnState != "" {
			upstreamReq.Header.Set("x-codex-turn-state", session.LastTurnState)
		}
		upstreamReq.Header.Set("originator", resolveCodexOriginator(req.Headers.Get("originator")))
	} else {
		// API Key 模式
		setAuthHeaders(upstreamReq, account)
		passHeadersForAccount(req.Headers, upstreamReq.Header, account)
	}

	return upstreamReq, nil
}

// handleAnthropicNonStreamFromResponses 非流式：聚合 Responses SSE → Anthropic JSON
// mappedModel: 映射后的 GPT 模型名，用于 Core 计费（写入 Usage.Model）
func (g *OpenAIGateway) handleAnthropicNonStreamFromResponses(
	resp *http.Response,
	w http.ResponseWriter,
	model string,
	mappedModel string,
	originalRequest []byte,
	requestServiceTier string,
	defaultServiceTier string,
	start time.Time,
	session openAISessionResolution,
	accountID int64,
) (sdk.ForwardOutcome, error) {
	timing := newResponseEventTiming(start)
	wsResult := ParseSSEStream(resp.Body, responseTimingObserver{timing: &timing})
	if wsResult.Err != nil {
		var failure *responsesFailureError
		if errors.As(wsResult.Err, &failure) {
			kind := failure.outcomeKind()
			body := anthropicErrorJSONWithCode(failure.AnthropicErrorType, failure.Code, failure.Message)
			return sdk.ForwardOutcome{
				Kind:           kind,
				FailoverScope:  failure.failoverScopeForKind(kind),
				Upstream:       sdk.UpstreamResponse{StatusCode: failure.StatusCode, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: body},
				Reason:         failure.upstreamReason(),
				RetryAfter:     failure.RetryAfter,
				Duration:       time.Since(start),
				SafetyRejected: failure.isCybersecurityRisk(),
			}, nil
		}
		// 非 *responsesFailureError 的 err（典型：SSE EOF 提前断流）→ UpstreamTransient，可 failover。
		outcome := transientOutcome(wsResult.Err.Error())
		outcome.SafetyRejected = isCybersecurityRiskRejectionText(string(wsResult.FailedEventRaw), wsResult.Err.Error())
		return outcome, wsResult.Err
	}
	if len(wsResult.CompletedEventRaw) == 0 {
		reason := "未收到 response.completed 事件"
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}

	// 客户端响应体使用原始 Claude 模型名；传入 wsResult 兜底用 delta 累积
	anthropicJSON := convertResponsesCompletedToAnthropicJSON(wsResult.CompletedEventRaw, originalRequest, model, &wsResult)
	if anthropicJSON == "" {
		reason := "responses 非流回译失败"
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}
	if session.SessionKey != "" && wsResult.ResponseID != "" {
		updateSessionStateResponseID(session.SessionKey, wsResult.ResponseID, accountID)
	}
	if session.SessionID != "" && session.DigestChain != "" {
		saveAnthropicDigestSession(accountID, session.DigestChain, session.SessionID, session.MatchedDigest)
	}

	if w != nil {
		setAnthropicStyleResponseHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicJSON))
	}
	anthropicBody := []byte(anthropicJSON)
	upstreamHeaders := http.Header{}
	upstreamHeaders.Set("Content-Type", "application/json")

	// 计费 model 用映射后的 GPT 名
	billingModel := mappedModel
	if billingModel == "" {
		billingModel = gjson.Get(anthropicJSON, "model").String()
	}
	serviceTier := firstNonEmptyTier(
		requestServiceTier,
		gjson.GetBytes(wsResult.CompletedEventRaw, "response.service_tier").String(),
		defaultServiceTier,
	)

	elapsed := time.Since(start)
	usage := newTokenUsage(
		billingModel,
		serviceTier,
		wsResult.InputTokens,
		wsResult.OutputTokens,
		wsResult.CachedInputTokens,
		wsResult.CacheCreationTokens,
		wsResult.ReasoningOutputTokens,
		timing.firstEventMs,
	)
	usage.FirstTokenMs = timing.firstTokenMs
	fillUsageCost(usage)
	setUsageResponseID(usage, wsResult.ResponseID)
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK, Headers: upstreamHeaders, Body: anthropicBody},
		Usage:    usage,
		Duration: elapsed,
	}, nil
}

// ──────────────────────────────────────────────────────
// 错误处理
// ──────────────────────────────────────────────────────

// writeAnthropicUpstreamError 将上游错误写入客户端（Anthropic 格式）并构造判决。
//
//   - 账号级错误（被禁用 / 限流 / 凭证失效，含 400 "organization disabled"）：
//     不写 w — 让 Core canFailover 不被"stream already written"短路，能切账号重试。
//   - 客户端错误：写 w（透传给客户端看到原始错误），Core 不 failover。
func (g *OpenAIGateway) writeAnthropicUpstreamError(
	w http.ResponseWriter,
	statusCode int,
	body []byte,
	start time.Time,
) (sdk.ForwardOutcome, error) {
	errMsg := extractOpenAIErrorMessage(body)
	if errMsg == "" {
		errMsg = truncate(string(body), 200)
	}

	kind := classifyAnthropicBody(statusCode, body)
	retryAfter := time.Duration(0)
	if statusCode == 429 {
		retryAfter = parseRetryDelay(errMsg)
	}

	errBody := anthropicErrorJSON(anthropicErrorType(statusCode), errMsg)

	outcome := sdk.ForwardOutcome{
		Kind:           kind,
		Upstream:       sdk.UpstreamResponse{StatusCode: statusCode, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: errBody},
		Reason:         errMsg,
		RetryAfter:     retryAfter,
		Duration:       time.Since(start),
		SafetyRejected: isCybersecurityRiskRejectionText(errMsg, string(body)),
	}

	if kind == sdk.OutcomeAccountRateLimited || kind == sdk.OutcomeAccountDead || kind == sdk.OutcomeUpstreamTransient {
		return outcome, fmt.Errorf("上游返回 %d: %s", statusCode, errMsg)
	}
	return outcome, nil
}

// extractOpenAIErrorMessage 从上游错误响应中提取错误消息（纯 gjson）
func extractOpenAIErrorMessage(body []byte) string {
	if msg := gjson.GetBytes(body, "error.message").String(); msg != "" {
		return msg
	}
	return ""
}
