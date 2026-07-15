package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/DevilGenius/airgate-openai/backend/internal/model"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// redactURL 去掉 query string，仅保留 host+path（避免敏感参数泄漏到日志）
func redactURL(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u != nil {
		u.RawQuery = ""
		u.User = nil
		return u.String()
	}
	if idx := strings.Index(rawURL, "?"); idx >= 0 {
		return rawURL[:idx]
	}
	return rawURL
}

// ──────────────────────────────────────────────────────
// 转发入口（三模式分发）
// ──────────────────────────────────────────────────────

// forwardHTTP 根据账号凭证类型分发到不同转发模式
func (g *OpenAIGateway) forwardHTTP(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	if isAnthropicCountTokensRequest(req) {
		return g.forwardAnthropicCountTokens(ctx, req)
	}

	// 检测 Anthropic Messages API 请求，走协议翻译
	if isAnthropicRequest(req) {
		return g.forwardAnthropicMessage(ctx, req)
	}

	// GET /v1/models：用插件内置模型清单本地返回。
	if isModelsListingRequest(req) {
		return buildLocalModelsResponse(), nil
	}

	if wireModel := req.DispatchPlan.UpstreamModel(); wireModel != "" {
		req.Model = wireModel
	}

	// 统一预处理请求体。multipart 请求（images/edits 上传图片）body 是二进制，
	// 不能按 JSON 处理否则会被 sjson 覆盖丢失数据。
	reqMethod, reqPath := resolveAPIKeyRoute(req)
	if denied, ok := enforceOperationPolicies(req, reqPath); ok {
		return denied, nil
	}
	if isResponsesCompactRequestPath(reqPath) && (req.Stream || gjson.GetBytes(req.Body, "stream").Bool()) {
		body := openAIErrorJSON("invalid_request_error", "invalid_request", "streaming not supported for /responses/compact")
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       body,
			},
			Reason: "streaming not supported for /responses/compact",
		}, nil
	}
	var reqServiceTier string
	if !strings.HasPrefix(req.Headers.Get("Content-Type"), "multipart/") {
		req.Body = preprocessRequestBody(req.Body, req.Model, reqPath, req.Headers)
		req.Body = applyForceInstructions(req.Body, req.Headers)
		if req.Account.Credentials["api_key"] != "" && isResponsesRequestPath(reqPath) {
			req.Body = normalizeResponsesToolCompatibility(req.Body)
		}
		reqServiceTier = normalizeOpenAIServiceTier(gjson.GetBytes(req.Body, "service_tier").String())
		if req.Account.Credentials["api_key"] != "" && req.Account.Credentials["access_token"] == "" {
			req.Body = applyOpenAIWireServiceTier(req.Body)
		}
	}

	account := req.Account

	// GET /v1/images/tasks/list：任务历史列表
	// GET /v1/images/tasks：任务状态查询
	// 两者都通过 image.generate handler 的 BuildResponse 投影响应。
	if isImageTaskListRequest(reqPath) || isImageTaskQuery(reqPath) {
		if g.host == nil {
			body := jsonError("任务系统未启用")
			return sdk.ForwardOutcome{
				Kind:     sdk.OutcomeClientError,
				Upstream: sdk.UpstreamResponse{StatusCode: http.StatusServiceUnavailable, Body: body},
			}, nil
		}
		handler := g.tasks.Get(taskTypeImageGenerate)
		if handler == nil {
			body := jsonError("任务类型未注册")
			return sdk.ForwardOutcome{
				Kind:     sdk.OutcomeClientError,
				Upstream: sdk.UpstreamResponse{StatusCode: http.StatusInternalServerError, Body: body},
			}, nil
		}
		if isImageTaskListRequest(reqPath) {
			return g.handleTaskList(ctx, req, handler)
		}
		return g.handleTaskQuery(ctx, req, handler)
	}

	if isImagesRequest(reqPath) {
		var cachedOutcome *sdk.ForwardOutcome
		ctx, cachedOutcome = g.checkImageSafetyRequest(ctx, req, reqMethod, reqPath)
		if cachedOutcome != nil {
			sdk.LoggerFromContext(ctx).Info("image_safety_request_cache_hit",
				sdk.LogFieldModel, req.Model,
				sdk.LogFieldPath, reqPath,
			)
			return *cachedOutcome, nil
		}
	}

	if !isImagesRequest(reqPath) && model.IsImageOnly(req.Model) {
		body := jsonError("图像模型不支持 Chat Completions，请使用 Images API")
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       body,
			},
			Reason: "图像模型不支持 chat completions",
		}, nil
	}

	// Images API 默认保持 OpenAI 标准同步响应，避免 SDK 把 task_id 响应解析成 data=nil。
	// 客户端显式发送 Prefer: respond-async 时，才创建 Core Task 并立即返回 202。
	if g.host != nil && isImagesRequest(reqPath) && !isTaskExecution(req.Headers) && prefersAsyncResponse(req.Headers) {
		taskType := taskTypeImageGenerate
		if isImagesEditRequest(reqPath) {
			taskType = taskTypeImageEdit
		}
		if handler := g.tasks.Get(taskType); handler != nil {
			return g.forwardTask(ctx, req, reqPath, handler)
		}
	}

	if account.Credentials["api_key"] != "" {
		return g.forwardAPIKey(ctx, req, reqServiceTier)
	}
	if account.Credentials["access_token"] != "" {
		if isImagesRequest(reqPath) {
			if shouldUseImagesWebReverse(account, req.Model) {
				return g.forwardImagesViaWebReverse(ctx, req)
			}
			return g.forwardImagesViaResponsesTool(ctx, req)
		}
		if isResponsesCompactRequestPath(reqPath) {
			return g.forwardOAuthCompact(ctx, req, reqServiceTier)
		}
		return g.forwardOAuth(ctx, req)
	}
	reason := "账号缺少 api_key 或 access_token"
	sdk.LoggerFromContext(ctx).Error("forward_dispatch_failed",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldReason, reason,
		sdk.LogFieldError, reason,
	)
	return accountDeadOutcome(reason), fmt.Errorf("%s", reason)
}

func (g *OpenAIGateway) forwardOAuthCompact(ctx context.Context, req *sdk.ForwardRequest, reqServiceTier string) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)
	session := resolveOpenAISession(req.Headers, req.Body, account.ID)
	updateSessionStateFromRequest(session, account.ID)
	upstreamBody := normalizePromptCacheKeyForUpstream(applyOpenAIWireReasoningEffort(req.Body, req.Model))

	targetURL := strings.TrimRight(ChatGPTSSEURL, "/") + "/compact"
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
	if err != nil {
		reason := fmt.Sprintf("构建上游请求失败: %v", err)
		logger.Warn("upstream_request_build_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			"url", redactURL(targetURL),
			sdk.LogFieldError, err,
		)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}

	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "application/json")
	upstreamReq.Header.Set("Authorization", "Bearer "+account.Credentials["access_token"])
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
	if ua := req.Headers.Get("User-Agent"); ua != "" {
		upstreamReq.Header.Set("User-Agent", ua)
	}

	logger.Debug("upstream_request_start",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		"url", redactURL(targetURL),
		sdk.LogFieldMethod, http.MethodPost,
		"stream", false,
		"account_type", "oauth",
	)

	resp, err := g.buildForwardHTTPClient(ctx, req, account).Do(upstreamReq)
	if err != nil {
		dur := time.Since(start)
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldError, err,
		)
		return transientOutcome(err.Error()), fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if snapshot := parseCodexUsageFromHeaders(resp.Header); snapshot != nil {
		StoreCodexUsage(account.ID, snapshot)
	}
	if turnState := decodeTurnStateHeader(resp.Header); turnState != "" {
		updateSessionStateTurnState(session.SessionKey, turnState)
	}

	if resp.StatusCode >= 400 {
		respBody, _ := readLimitedErrorBody(resp.Body)
		errDetail := gjson.GetBytes(respBody, "error.message").String()
		if errDetail == "" {
			errDetail = truncate(string(respBody), 200)
		}
		dur := time.Since(start)
		logger.Warn("upstream_request_non_2xx",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldStatus, resp.StatusCode,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldReason, errDetail,
		)
		outcome := failureOutcome(resp.StatusCode, respBody, resp.Header.Clone(), errDetail, extractRetryAfterHeader(resp.Header))
		outcome.Duration = dur
		return outcome, nil
	}

	logger.Debug("upstream_request_completed",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		sdk.LogFieldStatus, resp.StatusCode,
		sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
		"stream", false,
	)
	return handleNonStreamResponse(resp, req.Writer, start, reqServiceTier)
}

// isImageTaskListRequest 判断是否为 GET /v1/images/tasks/list 历史列表查询。
func isImageTaskListRequest(reqPath string) bool {
	return strings.HasSuffix(reqPath, "/images/tasks/list")
}

// isImageTaskQuery 判断是否为 GET /v1/images/tasks 任务状态查询。
func isImageTaskQuery(reqPath string) bool {
	return strings.HasSuffix(reqPath, "/images/tasks")
}

func prefersAsyncResponse(headers http.Header) bool {
	for _, value := range headers.Values("Prefer") {
		for _, token := range strings.Split(value, ",") {
			preference := strings.ToLower(strings.TrimSpace(token))
			if i := strings.IndexByte(preference, ';'); i >= 0 {
				preference = strings.TrimSpace(preference[:i])
			}
			if preference == "respond-async" {
				return true
			}
		}
	}
	return false
}

// isModelsListingRequest 判断当前请求是否为 GET /v1/models。
//
// Core 不会透传原始方法和路径，只能用既有线索推断：
//  1. 优先看 X-Forwarded-Path（如果 core 设置了）
//  2. 回退到 resolveAPIKeyRoute 的兜底推断（空 body + 非 stream → /v1/models）
//
// 这保持了与 resolveAPIKeyRoute 一致的推断逻辑，避免两处判据漂移。
func isModelsListingRequest(req *sdk.ForwardRequest) bool {
	if req == nil {
		return false
	}
	method, path := resolveAPIKeyRoute(req)
	return method == http.MethodGet && (path == "/v1/models" || strings.HasPrefix(path, "/v1/models?"))
}

// buildLocalModelsResponse 用插件内置模型注册表合成 OpenAI 兼容的 /v1/models 响应。
func buildLocalModelsResponse() sdk.ForwardOutcome {
	specs := model.AllSpecs(true)
	data := make([]map[string]any, 0, len(specs))
	created := time.Now().Unix()
	for _, spec := range specs {
		isImage := model.IsImageOnly(spec.ID)
		entry := map[string]any{
			"id":           spec.ID,
			"object":       "model",
			"created":      created,
			"owned_by":     "airgate",
			"capabilities": []string{"chat", "reasoning"},
		}
		if isImage {
			entry["capabilities"] = []string{"image_generation"}
			entry["image_only"] = true
		}
		if spec.ContextWindow > 0 {
			entry["context_window"] = spec.ContextWindow
			entry["context_length"] = spec.ContextWindow
			entry["max_input_tokens"] = spec.ContextWindow
		}
		if spec.MaxOutputTokens > 0 {
			entry["max_output_tokens"] = spec.MaxOutputTokens
		}
		data = append(data, entry)
	}
	body, _ := json.Marshal(map[string]any{
		"object": "list",
		"data":   data,
	})
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	return successOutcome(http.StatusOK, body, headers, nil)
}

// ──────────────────────────────────────────────────────
// API Key 模式：HTTP/SSE 直连上游
// ──────────────────────────────────────────────────────

func (g *OpenAIGateway) forwardAPIKey(ctx context.Context, req *sdk.ForwardRequest, reqServiceTier string) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)

	reqMethod, reqPath := resolveAPIKeyRoute(req)
	targetURL := buildAPIKeyURL(account, reqPath)
	isImageReq := isImagesRequest(reqPath)
	isImageEdit := isImagesEditRequest(reqPath)
	reqContentType := req.Headers.Get("Content-Type")
	isMultipart := isMultipartContentType(reqContentType)
	imagesRespOpts := imagesResponseOptions{IsEdit: isImageEdit}
	if isImageReq && len(req.Body) > 0 && (!isImageEdit || isMultipart) {
		imagesRespOpts = imagesResponseOptionsFromRequestBody(req.Body, reqContentType, isImageEdit)
	}
	if isImageEdit && len(req.Body) > 0 && !isMultipart {
		body, contentType, _, err := buildAPIKeyImagesEditMultipartBodyWithRequest(req.Body, reqContentType)
		if err != nil {
			errBody := jsonError(err.Error())
			return sdk.ForwardOutcome{
				Kind: sdk.OutcomeClientError,
				Upstream: sdk.UpstreamResponse{
					StatusCode: http.StatusBadRequest,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       errBody,
				},
				Reason:   err.Error(),
				Duration: time.Since(start),
			}, nil
		}
		req.Body = body
		req.Headers.Set("Content-Type", contentType)
		imagesRespOpts = imagesResponseOptionsFromRequestBody(body, contentType, true)
	} else if isImageEdit && len(req.Body) > 0 && isMultipart && req.Stream {
		body, contentType, err := stripMultipartFields(req.Body, reqContentType, "stream", "partial_images")
		if err != nil {
			errBody := jsonError(err.Error())
			return sdk.ForwardOutcome{
				Kind: sdk.OutcomeClientError,
				Upstream: sdk.UpstreamResponse{
					StatusCode: http.StatusBadRequest,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       errBody,
				},
				Reason:   err.Error(),
				Duration: time.Since(start),
			}, nil
		}
		req.Body = body
		req.Headers.Set("Content-Type", contentType)
		imagesRespOpts = imagesResponseOptionsFromRequestBody(body, contentType, true)
	} else if isImageReq && len(req.Body) > 0 && !isMultipart {
		for _, field := range []string{"stream", "partial_images"} {
			if patched, err := sjson.DeleteBytes(req.Body, field); err == nil {
				req.Body = patched
			}
		}
	}

	// 重启恢复：有上游异步 task_id 的 images 请求直接 poll，不再重复发起上游请求
	if isImagesRequest(reqPath) {
		if recoveryID := req.Headers.Get(upstreamTaskIDHeader); recoveryID != "" {
			logger.Info("images_async_task_recovery_before_request",
				sdk.LogFieldAccountID, account.ID,
				"upstream_task_id", recoveryID,
			)
			finalBody, pollErr := g.pollAsyncImageTask(ctx, account, recoveryID, logger)
			if pollErr == nil {
				mockResp := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader(finalBody)),
				}
				return g.handleImagesResponse(mockResp, req.Writer, nil, start, req.Model, imagesRespOpts)
			}
			if _, ok := normalizedImageSafetyFailureFromError(pollErr); ok {
				logger.Warn("images_async_task_recovery_safety_rejected",
					"upstream_task_id", recoveryID,
					sdk.LogFieldError, pollErr,
				)
				g.rememberImageSafetyRequest(ctx)
				outcome := imageSafetyClientOutcome()
				outcome.Duration = time.Since(start)
				return outcome, nil
			}
			reason := fmt.Sprintf("上游异步任务恢复失败: %v", pollErr)
			logger.Warn("images_async_task_recovery_failed",
				"upstream_task_id", recoveryID,
				sdk.LogFieldError, pollErr,
			)
			return transientOutcome(reason), fmt.Errorf("%s", reason)
		}
	}

	var sseKA *ssePingKeepAlive
	if isImagesRequest(reqPath) && req.Stream {
		sseKA = startSSEPingKeepAlive(req.Writer)
	}

	attempts := []imageSizeAttempt{{}}
	if isImageReq {
		attempts = imageSizeAttemptsForRequest(imagesRespOpts.RequestSize)
	}
	baseBody := append([]byte(nil), req.Body...)
	baseHeaders := req.Headers.Clone()
	logger.Debug("upstream_request_start",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		"url", redactURL(targetURL),
		sdk.LogFieldMethod, reqMethod,
		"stream", req.Stream,
		"account_type", "apikey",
	)

	streamable := req.Stream && req.Writer != nil && !isImageReq
	client := g.buildForwardHTTPClient(ctx, req, account)
	if streamable {
		client.Timeout = 0
	}
	recoveredPreviousResponse := false
	recoveredFunctionCallOutput := false
	for idx, attempt := range attempts {
		attemptBody, attemptContentType, err := imagesRequestBodyForAttempt(baseBody, baseHeaders.Get("Content-Type"), isImageEdit, attempt)
		if err != nil {
			if sseKA != nil {
				sseKA.Stop()
				writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
			}
			errBody := jsonError(err.Error())
			return sdk.ForwardOutcome{
				Kind: sdk.OutcomeClientError,
				Upstream: sdk.UpstreamResponse{
					StatusCode: http.StatusBadRequest,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       errBody,
				},
				Reason:   err.Error(),
				Duration: time.Since(start),
			}, nil
		}
		attemptHeaders := baseHeaders.Clone()
		if attemptContentType != "" {
			attemptHeaders.Set("Content-Type", attemptContentType)
		}
		attemptOpts := imagesResponseOptionsForAttempt(imagesRespOpts, attempt)
		if isImageReq && len(attemptBody) > 0 {
			attemptOpts = imagesResponseOptionsFromRequestBody(attemptBody, attemptContentType, isImageEdit)
			attemptOpts = imagesResponseOptionsForAttempt(attemptOpts, attempt)
		}
		finalAttempt := idx == len(attempts)-1
		outcome, err := g.forwardAPIKeyAttempt(ctx, req, reqMethod, reqPath, targetURL, attemptBody, attemptHeaders, attemptOpts, sseKA, start, reqServiceTier, finalAttempt, client)
		if !isImageReq && err == nil {
			delegatedRecoveryApplied := delegatedContinuationRecoveryApplied(attemptBody, baseHeaders)
			if !recoveredPreviousResponse && outcomeIsPreviousResponseNotFound(outcome) {
				if retryBody, ok := previousResponseNotFoundRecoveryBody(attemptBody); ok {
					recoveredPreviousResponse = true
					logger.Warn("previous_response_not_found_recovery_retry",
						sdk.LogFieldAccountID, account.ID,
						sdk.LogFieldModel, req.Model,
						sdk.LogFieldPath, reqPath,
						"account_type", "apikey",
					)
					outcome, err = g.forwardAPIKeyAttempt(ctx, req, reqMethod, reqPath, targetURL, retryBody, attemptHeaders, attemptOpts, sseKA, start, reqServiceTier, true, client)
					if err == nil {
						outcome, err = g.retryAPIKeyWithContextTooLargeFallback(
							ctx, req, outcome, reqMethod, reqPath, targetURL, retryBody, attemptHeaders, attemptOpts, sseKA, start, reqServiceTier, client,
							"previous_response_full_replay_context_too_large_compression_retry",
						)
					}
				}
			} else if !recoveredFunctionCallOutput && outcomeIsFunctionCallOutputWithoutCall(outcome) {
				if retryBody, ok := functionCallOutputRecoveryBody(attemptBody); ok {
					recoveredFunctionCallOutput = true
					logger.Warn("function_call_output_without_call_recovery_retry",
						sdk.LogFieldAccountID, account.ID,
						sdk.LogFieldModel, req.Model,
						sdk.LogFieldPath, reqPath,
						"account_type", "apikey",
					)
					outcome, err = g.forwardAPIKeyAttempt(ctx, req, reqMethod, reqPath, targetURL, retryBody, attemptHeaders, attemptOpts, sseKA, start, reqServiceTier, true, client)
					if err == nil {
						outcome, err = g.retryAPIKeyWithContextTooLargeFallback(
							ctx, req, outcome, reqMethod, reqPath, targetURL, retryBody, attemptHeaders, attemptOpts, sseKA, start, reqServiceTier, client,
							"function_call_output_recovery_context_too_large_compression_retry",
						)
					}
				}
			} else if delegatedRecoveryApplied && outcomeIsContextTooLarge(outcome) {
				outcome, err = g.retryAPIKeyWithContextTooLargeFallback(
					ctx, req, outcome, reqMethod, reqPath, targetURL, attemptBody, attemptHeaders, attemptOpts, sseKA, start, reqServiceTier, client,
					"delegated_full_replay_context_too_large_compression_retry",
				)
			} else if airgateContinuationRecoveryRequested(baseHeaders) && !delegatedRecoveryApplied && outcomeIsContextTooLarge(outcome) {
				logger.Warn("delegated_full_replay_context_too_large_not_recoverable",
					sdk.LogFieldAccountID, account.ID,
					sdk.LogFieldModel, req.Model,
					sdk.LogFieldPath, reqPath,
					"account_type", "apikey",
				)
			}
		}
		if outcome.Kind == sdk.OutcomeSuccess || finalAttempt || !isImageReq {
			return outcome, err
		}
		if !shouldRetryImageFallback(outcome, err) {
			if sseKA != nil {
				sseKA.Stop()
				writeImageOutcomeErrorSSEIfStarted(req.Writer, sseKA, outcome)
			}
			return outcome, err
		}
		logger.Warn("images_4k_downgrade_retry",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			"from_size", imagesRespOpts.RequestSize,
			"to_size", attempts[idx+1].Size,
			"next_retry", attempts[idx+1].RetryIndex,
			"kind", outcome.Kind,
			sdk.LogFieldStatus, outcome.Upstream.StatusCode,
			sdk.LogFieldReason, outcome.Reason,
		)
	}
	return transientOutcome("image request attempts exhausted"), fmt.Errorf("image request attempts exhausted")
}

func (g *OpenAIGateway) retryAPIKeyWithContextTooLargeFallback(
	ctx context.Context,
	req *sdk.ForwardRequest,
	outcome sdk.ForwardOutcome,
	reqMethod, reqPath, targetURL string,
	body []byte,
	headers http.Header,
	imagesRespOpts imagesResponseOptions,
	sseKA *ssePingKeepAlive,
	start time.Time,
	reqServiceTier string,
	client *http.Client,
	logMessage string,
) (sdk.ForwardOutcome, error) {
	// 流式响应立即透传，不为压缩模型降级保留响应缓存；最终失败交由 Trace 记录。
	if req.Stream || !outcomeIsContextTooLarge(outcome) || responseWriterWritten(req.Writer) {
		return outcome, nil
	}
	fallbackBody, fallbackModel, ok := responsesCompressionFallbackBody(body, req.Model)
	if !ok {
		return outcome, nil
	}
	fallbackReq := *req
	fallbackReq.Model = fallbackModel
	logger := sdk.LoggerFromContext(ctx)
	logger.Warn(logMessage,
		sdk.LogFieldAccountID, req.Account.ID,
		sdk.LogFieldModel, req.Model,
		sdk.LogFieldPath, reqPath,
		"fallback_model", fallbackModel,
		"account_type", "apikey",
	)
	return g.forwardAPIKeyAttempt(ctx, &fallbackReq, reqMethod, reqPath, targetURL, fallbackBody, headers, imagesRespOpts, sseKA, start, reqServiceTier, true, client)
}

func retryWSWithContextTooLargeFallback(
	ctx context.Context,
	accountID int64,
	sessionKey string,
	result WSResult,
	currentModel string,
	body []byte,
	w http.ResponseWriter,
	stream bool,
	streamOutputStarted func() bool,
	runAttempt func([]byte, http.ResponseWriter, string) (WSResult, error),
	logMessage string,
) (WSResult, string, error) {
	// WebSocket 流同样不延迟 response.created；仅非流式请求允许透明压缩降级。
	if stream || !isContextTooLargeErrorResult(result.Err) || streamOutputStarted() {
		return result, currentModel, nil
	}
	fallbackMsg, fallbackModel, ok := responsesCompressionFallbackBody(body, currentModel)
	if !ok {
		return result, currentModel, nil
	}
	logger := sdk.LoggerFromContext(ctx)
	logger.Warn(logMessage,
		sdk.LogFieldAccountID, accountID,
		sdk.LogFieldModel, currentModel,
		"fallback_model", fallbackModel,
		"account_type", "oauth",
		"session", sessionKey,
	)
	fallbackResult, err := runAttempt(fallbackMsg, w, fallbackModel)
	if err != nil {
		return result, fallbackModel, err
	}
	return fallbackResult, fallbackModel, nil
}

func (g *OpenAIGateway) forwardAPIKeyAttempt(
	ctx context.Context,
	req *sdk.ForwardRequest,
	reqMethod, reqPath, targetURL string,
	body []byte,
	headers http.Header,
	imagesRespOpts imagesResponseOptions,
	sseKA *ssePingKeepAlive,
	start time.Time,
	reqServiceTier string,
	finalAttempt bool,
	client *http.Client,
) (sdk.ForwardOutcome, error) {
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)

	if !isImagesRequest(reqPath) {
		body = applyOpenAIWireReasoningEffort(body, req.Model)
		body = normalizePromptCacheKeyForUpstream(body)
	}

	var bodyReader io.Reader
	if methodAllowsBody(reqMethod) && len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	upstreamReq, err := http.NewRequestWithContext(ctx, reqMethod, targetURL, bodyReader)
	if err != nil {
		reason := fmt.Sprintf("构建上游请求失败: %v", err)
		logger.Warn("upstream_request_build_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			"url", redactURL(targetURL),
			sdk.LogFieldError, err,
		)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}

	setAuthHeaders(upstreamReq, account)
	if methodAllowsBody(reqMethod) {
		// /v1/images/edits 是 multipart/form-data，必须保留 boundary。其它路径
		// Core 侧已把 body 归一化成 JSON 文本，统一 application/json。
		if ct := headers.Get("Content-Type"); isImagesEditRequest(reqPath) &&
			strings.HasPrefix(strings.ToLower(ct), "multipart/") {
			upstreamReq.Header.Set("Content-Type", ct)
		} else {
			upstreamReq.Header.Set("Content-Type", "application/json")
		}
	}
	passHeadersForAccount(headers, upstreamReq.Header, account)

	streamable := req.Stream && req.Writer != nil && !isImagesRequest(reqPath)
	resp, cancel, err := g.doStreamableUpstream(ctx, client, upstreamReq, streamable)
	if err != nil {
		dur := time.Since(start)
		if isImagesRequest(reqPath) {
			if _, ok := normalizedImageSafetyFailureFromError(err); ok {
				g.rememberImageSafetyRequest(ctx)
				logger.Warn("images_apikey_safety_error_normalized",
					sdk.LogFieldPath, reqPath,
					sdk.LogFieldModel, req.Model,
					sdk.LogFieldDurationMs, dur.Milliseconds(),
					sdk.LogFieldError, err,
				)
				if sseKA != nil && finalAttempt {
					sseKA.Stop()
					writeImageInvalidRequestSSEIfStarted(req.Writer, sseKA)
				}
				outcome := imageSafetyClientOutcome()
				outcome.Duration = dur
				return outcome, nil
			}
		}
		if sseKA != nil && finalAttempt {
			sseKA.Stop()
			logger.Warn("images_apikey_stream_failed_redacted",
				sdk.LogFieldPath, reqPath,
				sdk.LogFieldModel, req.Model,
				sdk.LogFieldError, err,
			)
			writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
		}
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldError, err,
		)
		// 网络层错误，无上游 HTTP 响应
		return transientOutcome(err.Error()), fmt.Errorf("请求上游失败: %w", err)
	}
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	// 非 2xx 统一走 failureOutcome 归类。包含 4xx（客户端错）/ 429 / 401 / 403 / 5xx。
	if resp.StatusCode >= 400 {
		respBody, _ := readLimitedErrorBody(resp.Body)
		errDetail := gjson.GetBytes(respBody, "error.message").String()
		if errDetail == "" {
			errDetail = truncate(string(respBody), 200)
		}
		dur := time.Since(start)
		clientStatus := resp.StatusCode
		clientBody := respBody
		clientHeaders := resp.Header.Clone()
		clientDetail := errDetail
		imageSafetyRejected := false
		if isImagesRequest(reqPath) {
			clientStatus, clientBody, clientHeaders, clientDetail, imageSafetyRejected = normalizeImageUpstreamError(
				resp.StatusCode,
				respBody,
				clientHeaders,
				errDetail,
			)
			if imageSafetyRejected {
				g.rememberImageSafetyRequest(ctx)
			}
		}
		if sseKA != nil && finalAttempt {
			sseKA.Stop()
			logger.Warn("images_apikey_upstream_error_redacted",
				sdk.LogFieldPath, reqPath,
				sdk.LogFieldModel, req.Model,
				sdk.LogFieldStatus, resp.StatusCode,
				sdk.LogFieldReason, errDetail,
			)
			if imageSafetyRejected {
				writeImageInvalidRequestSSEIfStarted(req.Writer, sseKA)
			} else {
				writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
			}
		}
		logger.Warn("upstream_request_non_2xx",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldStatus, resp.StatusCode,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldReason, errDetail,
		)
		outcome := failureOutcome(clientStatus, clientBody, clientHeaders, clientDetail, extractRetryAfterHeader(clientHeaders))
		if isImagesRequest(reqPath) {
			applyImageRateLimitPolicy(&outcome)
		}
		outcome.Duration = time.Since(start)
		return outcome, nil
	}

	logger.Debug("upstream_request_completed",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		sdk.LogFieldStatus, resp.StatusCode,
		sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
		"content_length", resp.ContentLength,
		"stream", req.Stream,
	)

	// /v1/models 路径补齐上下文元信息
	if reqMethod == http.MethodGet && strings.HasPrefix(reqPath, "/v1/models") {
		resp = enrichModelsResponse(resp)
	}

	// 捕获上游 Codex 用量头
	if snapshot := parseCodexUsageFromHeaders(resp.Header); snapshot != nil {
		StoreCodexUsage(account.ID, snapshot)
	}

	// Images API 响应体无 model 字段，另走专用处理器回填后再 fillCost
	if isImagesRequest(reqPath) {
		// 检测异步任务模式（如 apimart.ai gpt-image-2 返回 task_id）
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			reason := fmt.Sprintf("读取 Images 响应失败: %v", readErr)
			if sseKA != nil && finalAttempt {
				sseKA.Stop()
				writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
			}
			return transientOutcome(reason), fmt.Errorf("%s", reason)
		}

		if taskID, ok := isAsyncImageTaskResponse(body); ok {
			logger.Debug("images_async_task_detected",
				sdk.LogFieldAccountID, account.ID,
				sdk.LogFieldModel, req.Model,
				"task_id", taskID,
			)
			// 保存上游 task_id 到 core task 的 execution，服务重启后可恢复
			if coreTaskID := req.Headers.Get(taskIDHeader); coreTaskID != "" {
				cid, _ := strconv.ParseInt(coreTaskID, 10, 64)
				if cid > 0 {
					_ = g.updateHostTask(ctx, cid, "", 0, nil, "",
						WithExecution(map[string]any{"upstream_task_id": taskID}))
				}
			}

			finalBody, pollErr := g.pollAsyncImageTask(ctx, account, taskID, logger)
			if pollErr != nil {
				logger.Warn("images_async_task_poll_failed",
					sdk.LogFieldAccountID, account.ID,
					sdk.LogFieldModel, req.Model,
					"task_id", taskID,
					sdk.LogFieldError, pollErr,
				)
				if _, ok := normalizedImageSafetyFailureFromError(pollErr); ok {
					g.rememberImageSafetyRequest(ctx)
					outcome := imageSafetyClientOutcome()
					outcome.Duration = time.Since(start)
					if sseKA != nil && finalAttempt {
						sseKA.Stop()
						writeImageInvalidRequestSSEIfStarted(req.Writer, sseKA)
					}
					return outcome, nil
				}
				reason := fmt.Sprintf("异步图片任务轮询失败: %v", pollErr)
				if sseKA != nil && finalAttempt {
					sseKA.Stop()
					writeSSEErrorIfStarted(req.Writer, sseKA, sanitizedImageSSEErrorMessage)
				}
				return transientOutcome(reason), fmt.Errorf("%s", reason)
			}
			body = finalBody
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return g.handleImagesResponse(resp, req.Writer, sseKA, start, req.Model, imagesRespOpts)
	}

	if req.Stream && req.Writer != nil {
		return handleStreamResponseWithLogger(logger, resp, req.Writer, start, reqServiceTier)
	}
	return handleNonStreamResponse(resp, req.Writer, start, reqServiceTier)
}

func enrichModelsResponse(resp *http.Response) *http.Response {
	if resp == nil || resp.Body == nil {
		return resp
	}

	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || len(raw) == 0 {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		if len(raw) > 0 {
			resp.ContentLength = int64(len(raw))
		}
		return resp
	}

	dataNode := gjson.GetBytes(raw, "data")
	if !dataNode.Exists() || !dataNode.IsArray() {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		resp.ContentLength = int64(len(raw))
		return resp
	}

	updated := raw
	changed := false
	for idx, item := range dataNode.Array() {
		modelID := strings.TrimSpace(item.Get("id").String())
		if modelID == "" {
			continue
		}

		meta := getModelMetadataByID(modelID)
		if len(meta) == 0 {
			continue
		}
		for key, value := range meta {
			path := fmt.Sprintf("data.%d.%s", idx, key)
			if gjson.GetBytes(updated, path).Exists() {
				continue
			}
			patched, setErr := sjson.SetBytes(updated, path, value)
			if setErr != nil {
				continue
			}
			updated = patched
			changed = true
		}
	}

	if !changed {
		updated = raw
	}

	resp.Body = io.NopCloser(bytes.NewReader(updated))
	resp.ContentLength = int64(len(updated))
	if resp.Header != nil {
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(updated)))
		resp.Header.Set("Content-Type", "application/json")
	}
	return resp
}

// ──────────────────────────────────────────────────────
// OAuth 模式：WebSocket 连上游，SSE 写回客户端
// ──────────────────────────────────────────────────────

// forwardOAuth 使用 WebSocket 连接上游，将响应以 SSE 格式写回客户端
func (g *OpenAIGateway) forwardOAuth(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)
	session := resolveOpenAISession(req.Headers, req.Body, account.ID)
	updateSessionStateFromRequest(session, account.ID)

	cfg := WSConfig{
		Token:          account.Credentials["access_token"],
		AccountID:      account.Credentials["chatgpt_account_id"],
		ProxyURL:       account.ProxyURL,
		SessionID:      session.SessionID,
		ConversationID: session.ConversationID,
		TurnState:      session.LastTurnState,
		Originator:     resolveCodexOriginator(req.Headers.Get("originator")),
	}

	logger.Debug("upstream_request_start",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		"url", "wss://chatgpt.com/backend-api/codex",
		sdk.LogFieldMethod, "WS",
		"stream", req.Stream,
		"account_type", "oauth",
	)

	wsDialStart := time.Now()
	conn, wsResp, err := DialWebSocket(ctx, cfg)
	wsDialMs := time.Since(wsDialStart).Milliseconds()
	if err != nil {
		dur := time.Since(start)
		// WS 握手失败：按 HTTP 响应码归类。无响应则视为网络层 transient。
		if wsResp != nil {
			logger.Warn("upstream_request_non_2xx",
				sdk.LogFieldAccountID, account.ID,
				sdk.LogFieldStatus, wsResp.StatusCode,
				sdk.LogFieldDurationMs, dur.Milliseconds(),
				sdk.LogFieldReason, err.Error(),
				"phase", "ws_handshake",
			)
			wsBody := openAIErrorJSON(openAIErrorTypeForStatus(wsResp.StatusCode), "", err.Error())
			outcome := failureOutcome(wsResp.StatusCode, wsBody, wsResp.Header.Clone(), err.Error(), extractRetryAfterHeader(wsResp.Header))
			outcome.Duration = time.Since(start)
			return outcome, forwardErrForOutcome(outcome, err)
		}
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldDurationMs, dur.Milliseconds(),
			sdk.LogFieldError, err,
			"phase", "ws_handshake",
		)
		return transientOutcome(err.Error()), err
	}
	defer func() { _ = conn.Close() }()
	if wsResp != nil {
		if turnState := decodeTurnStateHeader(wsResp.Header); turnState != "" {
			updateSessionStateTurnState(session.SessionKey, turnState)
		}
	}

	// 构建 response.create 消息
	if airgateContinuationRecoveryRequested(req.Headers) {
		session.PreviousRespID = ""
	}
	createMsg, err := g.buildWSRequest(req, session)
	if err != nil {
		reason := fmt.Sprintf("构建 WebSocket 请求失败: %v", err)
		logger.Warn("ws_build_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldError, err,
		)
		outcome := sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       openAIErrorJSON("invalid_request_error", "invalid_request", reason),
			},
			Reason:   reason,
			Duration: time.Since(start),
		}
		return outcome, forwardErrForOutcome(outcome, fmt.Errorf("%s", reason))
	}

	// 协议分叉：客户端如果走的是 /v1/chat/completions（而不是原生 /v1/responses），
	// OAuth 上游依然只吐 Responses API 的 SSE 事件——需要把这些事件翻译回 Chat
	// Completions 协议，否则标准 OpenAI SDK 的 chat.completions.create 无法解析。
	isChatCompletions := isChatCompletionsRequest(req)
	chatStreamInclude := isChatCompletions && req.Stream &&
		gjson.GetBytes(req.Body, "stream_options.include_usage").Bool()

	// 是否走静默缓冲路径（等整条响应就绪再吐 JSON）。两种场景触发：
	//   - /v1/chat/completions 非流式
	//   - /v1/responses 非流式
	silentBuffered := !req.Stream

	var (
		lastSSEHandler      *sseEventWriter
		lastChatWriter      *chatCompletionsStreamWriter
		lastChatSilent      *chatCompletionsSilentHandler
		lastResponsesSilent *responsesSilentHandler
	)

	currentModel := req.Model
	runAttempt := func(msg []byte, w http.ResponseWriter, attemptModel string) (WSResult, error) {
		if req.TraceFinalError {
			captureFinalErrorWebSocketRequest(ctx, ChatGPTWSURL, cfg, msg)
		}
		if err := writeWebSocketJSON(conn, json.RawMessage(msg)); err != nil {
			return WSResult{}, fmt.Errorf("发送 WebSocket 消息失败: %w", err)
		}
		lastSSEHandler = nil
		lastChatWriter = nil
		lastChatSilent = nil
		lastResponsesSilent = nil
		var handler WSEventHandler
		switch {
		case isChatCompletions && req.Stream:
			writer := newChatCompletionsStreamWriter(
				w, attemptModel, account.ID, session.SessionKey, chatStreamInclude, start,
			)
			lastChatWriter = writer
			handler = writer
		case isChatCompletions && !req.Stream:
			silent := &chatCompletionsSilentHandler{
				accountID:  account.ID,
				sessionKey: session.SessionKey,
				timing:     newResponseEventTiming(start),
			}
			lastChatSilent = silent
			handler = silent
		case !isChatCompletions && !req.Stream:
			// 原生 /v1/responses 非流式：缓冲事件，末尾一次性吐 JSON
			silent := &responsesSilentHandler{
				accountID:  account.ID,
				sessionKey: session.SessionKey,
				timing:     newResponseEventTiming(start),
			}
			lastResponsesSilent = silent
			handler = silent
		default:
			// 原生 /v1/responses 流式：SSE 透传
			sseHandler := &sseEventWriter{
				w:          w,
				accountID:  account.ID,
				sessionKey: session.SessionKey,
				timing:     newResponseEventTiming(start),
			}
			if f, ok := w.(http.Flusher); ok {
				sseHandler.flusher = f
			}
			lastSSEHandler = sseHandler
			handler = sseHandler
		}
		result := ReceiveWSResponse(ctx, conn, handler)
		if req.TraceFinalError && len(result.FailedEventRaw) > 0 {
			captureFinalErrorUpstreamBody(ctx, result.FailedEventRaw)
		}
		return result, nil
	}

	w := req.Writer

	// 流式响应先设置头，但不提前 WriteHeader。
	// 如果上游在首个可转发事件前返回校验错误（例如模型不支持），调用方仍能写出 4xx JSON。
	if w != nil && !silentBuffered {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
	}

	delegatedRecoveryApplied := delegatedContinuationRecoveryApplied(createMsg, req.Headers)
	result, err := runAttempt(createMsg, w, currentModel)
	if err != nil {
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
			sdk.LogFieldError, err,
			"phase", "ws_send",
		)
		return transientOutcome(err.Error()), err
	}
	streamOutputStarted := func() bool {
		return (lastChatWriter != nil && lastChatWriter.wrote) ||
			(lastSSEHandler != nil && lastSSEHandler.wrote)
	}
	if delegatedRecoveryApplied {
		result, currentModel, err = retryWSWithContextTooLargeFallback(
			ctx, account.ID, session.SessionKey, result, currentModel, createMsg, w, req.Stream, streamOutputStarted, runAttempt,
			"delegated_full_replay_context_too_large_compression_retry",
		)
		if err != nil {
			logger.Warn("upstream_request_failed",
				sdk.LogFieldAccountID, account.ID,
				sdk.LogFieldModel, currentModel,
				sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
				sdk.LogFieldError, err,
				"phase", "ws_compression_retry_send",
			)
			return transientOutcome(err.Error()), err
		}
	} else if airgateContinuationRecoveryRequested(req.Headers) && !delegatedRecoveryApplied && isContextTooLargeErrorResult(result.Err) && !streamOutputStarted() {
		logger.Warn("delegated_full_replay_context_too_large_not_recoverable",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			"account_type", "oauth",
			"session", session.SessionKey,
		)
	}
	if isPreviousResponseNotFoundError(result.Err) && !streamOutputStarted() {
		if retryMsg, ok := previousResponseNotFoundRecoveryBody(createMsg); ok {
			logger.Warn("previous_response_not_found_recovery_retry",
				sdk.LogFieldAccountID, account.ID,
				sdk.LogFieldModel, req.Model,
				"account_type", "oauth",
				"session", session.SessionKey,
			)
			clearSessionStateResponseID(session.SessionKey)
			result, err = runAttempt(retryMsg, w, currentModel)
			if err != nil {
				logger.Warn("upstream_request_failed",
					sdk.LogFieldAccountID, account.ID,
					sdk.LogFieldModel, req.Model,
					sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
					sdk.LogFieldError, err,
					"phase", "ws_recovery_send",
				)
				return transientOutcome(err.Error()), err
			}
			result, currentModel, err = retryWSWithContextTooLargeFallback(
				ctx, account.ID, session.SessionKey, result, currentModel, retryMsg, w, req.Stream, streamOutputStarted, runAttempt,
				"previous_response_full_replay_context_too_large_compression_retry",
			)
			if err != nil {
				logger.Warn("upstream_request_failed",
					sdk.LogFieldAccountID, account.ID,
					sdk.LogFieldModel, currentModel,
					sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
					sdk.LogFieldError, err,
					"phase", "ws_compression_retry_send",
				)
				return transientOutcome(err.Error()), err
			}
		}
	}
	if isFunctionCallOutputWithoutCallErrorResult(result.Err) && !streamOutputStarted() {
		if retryMsg, ok := functionCallOutputRecoveryBody(createMsg); ok {
			logger.Warn("function_call_output_without_call_recovery_retry",
				sdk.LogFieldAccountID, account.ID,
				sdk.LogFieldModel, req.Model,
				"account_type", "oauth",
				"session", session.SessionKey,
			)
			clearSessionStateResponseID(session.SessionKey)
			result, err = runAttempt(retryMsg, w, currentModel)
			if err != nil {
				logger.Warn("upstream_request_failed",
					sdk.LogFieldAccountID, account.ID,
					sdk.LogFieldModel, req.Model,
					sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
					sdk.LogFieldError, err,
					"phase", "ws_function_call_output_recovery_send",
				)
				return transientOutcome(err.Error()), err
			}
			result, currentModel, err = retryWSWithContextTooLargeFallback(
				ctx, account.ID, session.SessionKey, result, currentModel, retryMsg, w, req.Stream, streamOutputStarted, runAttempt,
				"function_call_output_recovery_context_too_large_compression_retry",
			)
			if err != nil {
				logger.Warn("upstream_request_failed",
					sdk.LogFieldAccountID, account.ID,
					sdk.LogFieldModel, currentModel,
					sdk.LogFieldDurationMs, time.Since(start).Milliseconds(),
					sdk.LogFieldError, err,
					"phase", "ws_compression_retry_send",
				)
				return transientOutcome(err.Error()), err
			}
		}
	}
	if session.SessionKey != "" {
		if result.ResponseID != "" {
			updateSessionStateResponseID(session.SessionKey, result.ResponseID, account.ID)
		}
	}

	elapsed := time.Since(start)
	firstEventMs := elapsed.Milliseconds()
	var firstTokenMs int64
	switch {
	case lastChatWriter != nil:
		firstEventMs = lastChatWriter.timing.firstEventMs
		firstTokenMs = lastChatWriter.timing.firstTokenMs
	case lastSSEHandler != nil:
		firstEventMs = lastSSEHandler.timing.firstEventMs
		firstTokenMs = lastSSEHandler.timing.firstTokenMs
	case lastChatSilent != nil:
		firstEventMs = lastChatSilent.timing.firstEventMs
		firstTokenMs = lastChatSilent.timing.firstTokenMs
	case lastResponsesSilent != nil:
		firstEventMs = lastResponsesSilent.timing.firstEventMs
		firstTokenMs = lastResponsesSilent.timing.firstTokenMs
	}
	if firstEventMs <= 0 {
		firstEventMs = elapsed.Milliseconds()
	}

	usage := newTokenUsage(
		firstNonEmptyString(result.Model, currentModel),
		normalizeOpenAIServiceTier(gjson.GetBytes(req.Body, "service_tier").String()),
		result.InputTokens,
		result.OutputTokens,
		result.CachedInputTokens,
		result.CacheCreationTokens,
		result.ReasoningOutputTokens,
		firstEventMs,
	)
	usage.FirstTokenMs = firstTokenMs
	usage.WSDialMs = wsDialMs
	numImages := len(result.ImageGenCalls)
	imageToolSize := ""
	if numImages > 0 {
		imageToolSize = result.ImageGenCalls[0].Size
	}

	if result.Err != nil {
		var failure *responsesFailureError
		kind := sdk.OutcomeUpstreamTransient
		statusCode := http.StatusBadGateway
		message := result.Err.Error()
		var retryAfter time.Duration
		code := kind.String()
		failoverScope := sdk.FailoverScopeNone
		if errors.As(result.Err, &failure) {
			kind = failure.outcomeKind()
			statusCode = failure.StatusCode
			message = failure.Message
			retryAfter = failure.RetryAfter
			code = failure.codeOrKind()
			failoverScope = failure.failoverScopeForKind(kind)
		}
		// 流已经提交后不能再由 Core 改写 HTTP 状态或切换 dispatch 候选。
		// 原生 Responses 流如果还没有转发过上游错误事件，则在流内补一个终止错误事件。
		if req.Stream && streamOutputStarted() {
			failoverScope = sdk.FailoverScopeNone
			if kind != sdk.OutcomeClientError {
				kind = sdk.OutcomeStreamAborted
				code = kind.String()
			}
			if lastSSEHandler != nil {
				lastSSEHandler.writeTerminalErrorIfNeeded(statusCode, code, message, result.ResponseID)
			}
		}
		errBody := openAIErrorJSON(openAIErrorTypeForStatus(statusCode), code, message)
		logger.Warn("upstream_request_non_2xx",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldStatus, statusCode,
			sdk.LogFieldDurationMs, elapsed.Milliseconds(),
			sdk.LogFieldReason, message,
			"phase", "ws_response",
			"ws_event_count", result.EventCount,
			"ws_token_event_count", result.TokenEventCount,
			"ws_first_event", result.FirstEventType,
			"ws_last_event", result.LastEventType,
			"stream_output_started", streamOutputStarted(),
		)
		outcome := sdk.ForwardOutcome{
			Kind:          kind,
			FailoverScope: failoverScope,
			Upstream:      sdk.UpstreamResponse{StatusCode: statusCode, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: errBody},
			Reason:        message,
			RetryAfter:    retryAfter,
			Duration:      elapsed,
		}
		// 即使请求失败，上游可能已消耗 token（如 response.failed / response.incomplete），
		// 仍需计费避免漏洞。
		if result.InputTokens > 0 || result.OutputTokens > 0 || result.CachedInputTokens > 0 || result.CacheCreationTokens > 0 {
			fillUsageCostWithImageTool(usage, numImages, imageToolSize)
			outcome.Usage = usage
		}
		return outcome, forwardErrForOutcome(outcome, result.Err)
	}

	// 结束标记 / 响应体写回。必须在 result.Err 判定之后执行，避免把上游错误补成
	// finish_reason=stop + [DONE] 的空成功流。
	switch {
	case isChatCompletions && req.Stream:
		if lastChatWriter != nil {
			lastChatWriter.finalize()
		}
	case isChatCompletions && !req.Stream:
		if w != nil {
			body := buildNonStreamChatCompletion(result, firstNonEmptyString(result.Model, req.Model))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
	case !isChatCompletions && !req.Stream:
		// /v1/responses 非流式：从 WSResult 抽 response 字段回写 JSON
		if w != nil {
			body := buildNonStreamResponses(result)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
	default:
		// /v1/responses 流式：补 [DONE] 标记
		if w != nil {
			if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err == nil {
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}

	logger.Debug("upstream_request_completed",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		sdk.LogFieldStatus, http.StatusOK,
		sdk.LogFieldDurationMs, elapsed.Milliseconds(),
		"first_event_ms", firstEventMs,
		"first_token_ms", firstTokenMs,
		"ws_dial_ms", wsDialMs,
		"input_tokens", result.InputTokens,
		"output_tokens", result.OutputTokens,
		"stream", req.Stream,
	)
	fillUsageCostWithImageTool(usage, numImages, imageToolSize)
	setUsageResponseID(usage, result.ResponseID)
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK},
		Usage:    usage,
		Duration: elapsed,
	}, nil
}

// ──────────────────────────────────────────────────────
// SSE 事件写入器（WSEventHandler 实现）
// ──────────────────────────────────────────────────────

// sseEventWriter 将 WS 事件转为 SSE 格式写入 http.ResponseWriter
type sseEventWriter struct {
	w                      http.ResponseWriter
	flusher                http.Flusher
	accountID              int64 // 用于存储 Codex 用量快照
	sessionKey             string
	timing                 responseEventTiming
	wrote                  bool
	terminalErrorForwarded bool
}

func (s *sseEventWriter) OnTextDelta(string)      {}
func (s *sseEventWriter) OnReasoningDelta(string) {}
func (s *sseEventWriter) OnRateLimits(used float64) {
	if s.accountID > 0 {
		StoreCodexUsage(s.accountID, &CodexUsageSnapshot{
			PrimaryUsedPercent: used,
			CapturedAt:         time.Now(),
		})
	}
}

func (s *sseEventWriter) OnRawEvent(eventType string, data []byte) {
	if s.w == nil || eventType == "" {
		return
	}
	terminalErrorEvent := eventType == "response.failed" || eventType == "error"
	s.timing.observe(eventType, data)
	// 过滤不需要转发给客户端的内部事件，并捕获用量
	switch eventType {
	case "codex.rate_limits":
		if s.accountID > 0 {
			if snapshot := parseCodexUsageFromSSEEvent(data); snapshot != nil {
				StoreCodexUsage(s.accountID, snapshot)
			}
		}
		return
	case "response.failed", "error":
		if !s.wrote {
			return
		}
	case "response.created", "response.completed", "response.done":
		if s.sessionKey != "" {
			if responseID := gjson.GetBytes(data, "response.id").String(); strings.TrimSpace(responseID) != "" {
				updateSessionStateResponseID(s.sessionKey, responseID, s.accountID)
			}
		}
	}
	if _, err := fmt.Fprint(s.w, formatSSEEvent(eventType, data)); err != nil {
		return
	}
	s.wrote = true
	if terminalErrorEvent {
		s.terminalErrorForwarded = true
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
}
func formatSSEEvent(eventType string, data []byte) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, strings.ReplaceAll(string(data), "\n", ""))
}

func (s *sseEventWriter) writeTerminalErrorIfNeeded(statusCode int, code, message, responseID string) {
	if s == nil || s.w == nil || !s.wrote || s.terminalErrorForwarded {
		return
	}
	code = strings.TrimSpace(code)
	if code == "" {
		code = "upstream_error"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "请求暂时无法完成，请稍后重试"
	}
	payload := `{"type":"response.failed","response":{"status":"failed","error":{"message":"","type":"","code":""}}}`
	payload, _ = sjson.Set(payload, "response.error.message", message)
	payload, _ = sjson.Set(payload, "response.error.type", openAIErrorTypeForStatus(statusCode))
	payload, _ = sjson.Set(payload, "response.error.code", code)
	if responseID = strings.TrimSpace(responseID); responseID != "" {
		payload, _ = sjson.Set(payload, "response.id", responseID)
	}
	if _, err := fmt.Fprintf(s.w, "event: response.failed\ndata: %s\n\n", strings.ReplaceAll(payload, "\n", "")); err != nil {
		return
	}
	s.terminalErrorForwarded = true
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// ──────────────────────────────────────────────────────
// HTTP 客户端
// ──────────────────────────────────────────────────────

func methodAllowsBody(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

type responseWriterWrittenChecker interface {
	Written() bool
}

func responseWriterWritten(w http.ResponseWriter) bool {
	if w == nil {
		return false
	}
	if checker, ok := w.(responseWriterWrittenChecker); ok {
		return checker.Written()
	}
	return false
}

// requestTimeout 获取插件默认请求超时配置
func (g *OpenAIGateway) requestTimeout() time.Duration {
	const fallback = 300 * time.Second
	if g == nil || g.ctx == nil || g.ctx.Config() == nil {
		return fallback
	}
	timeout := g.ctx.Config().GetDuration("default_timeout")
	if timeout <= 0 {
		return fallback
	}
	return timeout
}

// buildHTTPClient 构建带代理支持的 HTTP 客户端
// 使用 TransportPool 按账户+代理隔离连接，同一账户复用连接
func (g *OpenAIGateway) buildHTTPClient(account *sdk.Account) *http.Client {
	if g == nil || g.transportPool == nil {
		return &http.Client{
			Timeout: g.requestTimeout(),
		}
	}
	transport := g.transportPool.GetTransport(account.ID, account.ProxyURL)

	return &http.Client{
		Transport: transport,
		Timeout:   g.requestTimeout(),
	}
}

const (
	defaultFirstByteTimeout  = 60 * time.Second
	defaultStreamIdleTimeout = 60 * time.Second
)

func (g *OpenAIGateway) firstByteTimeout() time.Duration {
	if g == nil || g.ctx == nil || g.ctx.Config() == nil {
		return defaultFirstByteTimeout
	}
	if d := g.ctx.Config().GetDuration("first_byte_timeout"); d > 0 {
		return d
	}
	return defaultFirstByteTimeout
}

func (g *OpenAIGateway) streamIdleTimeout() time.Duration {
	if g == nil || g.ctx == nil || g.ctx.Config() == nil {
		return defaultStreamIdleTimeout
	}
	if d := g.ctx.Config().GetDuration("stream_idle_timeout"); d > 0 {
		return d
	}
	return defaultStreamIdleTimeout
}

func (g *OpenAIGateway) doStreamableUpstream(ctx context.Context, client *http.Client, upstreamReq *http.Request, stream bool) (*http.Response, context.CancelFunc, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	upstreamReq = upstreamReq.WithContext(reqCtx)

	var firstByteTimer *time.Timer
	if stream {
		client.Timeout = 0
		firstByteTimer = time.AfterFunc(g.firstByteTimeout(), cancel)
	}

	resp, err := client.Do(upstreamReq)
	if firstByteTimer != nil {
		firstByteTimer.Stop()
	}
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if stream {
		resp.Body = newStallGuardBody(resp.Body, g.streamIdleTimeout(), cancel)
	}
	return resp, cancel, nil
}
