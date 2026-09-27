package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/DevilGenius/airgate-openai/backend/internal/basispoints"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const basispointsSettingHeader = "X-Airgate-Plugin-Openai-Basispoints"

func basispointsEnabled(req *sdk.ForwardRequest) bool {
	return req != nil && req.Account != nil &&
		strings.EqualFold(strings.TrimSpace(req.Headers.Get(basispointsSettingHeader)), "true") &&
		req.Account.Credentials["api_key"] == "" &&
		isOpenAIOAuthCredentials(req.Account.Credentials) && !isOpenAIAgentIdentityAccount(req.Account)
}

func basispointsIdentity(req *sdk.ForwardRequest, body []byte) string {
	// Include both tenant and account identities. Hash client-controlled values to
	// avoid delimiter collisions and keep identity seeds bounded. Do not use native
	// previous_response_id/session state: BPS always receives expanded history.
	thread := firstNonEmptyString(req.Headers.Get("session_id"), req.Headers.Get("conversation_id"), gjson.GetBytes(body, "prompt_cache_key").String())
	if thread == "" {
		thread = gjson.GetBytes(body, "input.0").Raw
	}
	identity := []string{fmt.Sprint(req.Account.ID), req.Account.Credentials["chatgpt_account_id"], req.Headers.Get("X-Airgate-User-ID"), req.Headers.Get("X-Airgate-API-Key-ID"), req.Headers.Get("X-Airgate-Group-ID"), thread}
	var scope strings.Builder
	for _, value := range identity {
		fmt.Fprintf(&scope, "%d:%s", len(value), value)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(scope.String())))
}

type basispointsBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *basispointsBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

// openBasispoints returns used=false only before any client output, either for
// local incompatibility or an explicit HTTP model rejection. The native caller
// retains its original request and cannot recursively re-enter this attempt.
func (g *OpenAIGateway) openBasispoints(ctx context.Context, req *sdk.ForwardRequest, body []byte) (*http.Response, bool, error) {
	if !basispointsEnabled(req) {
		return nil, false, nil
	}
	prepared, reason := basispoints.PrepareRequest(body, basispointsIdentity(req, body))
	if reason != "" {
		sdk.LoggerFromContext(ctx).Info("basispoints_native_route", "reason", reason, "model", req.Model)
		return nil, false, nil
	}
	auth, err := g.buildOpenAIAuthHeaders(ctx, req.Account, false)
	if err != nil {
		return nil, true, err
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, basispoints.ResponsesURL, prepared.Body())
	if err != nil {
		return nil, true, err
	}
	upstream.Header = basispoints.Headers(auth.Get("Authorization"), req.Account.Credentials["chatgpt_account_id"])
	client := g.buildForwardHTTPClient(ctx, req, req.Account)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, cancel, err := g.doStreamableUpstream(ctx, client, upstream, true)
	if err != nil {
		return nil, true, err
	}
	if resp.StatusCode >= 300 {
		errorBody, readErr := readLimitedErrorBody(resp.Body)
		_ = resp.Body.Close()
		cancel()
		if readErr != nil {
			return nil, true, readErr
		}
		if basispoints.ModelUnavailable(resp.StatusCode, errorBody) {
			sdk.LoggerFromContext(ctx).Info("basispoints_native_route", "reason", "model_unavailable", "model", req.Model)
			return nil, false, nil
		}
		resp.Body = io.NopCloser(bytes.NewReader(errorBody))
		return resp, true, nil
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		_ = resp.Body.Close()
		cancel()
		return nil, true, fmt.Errorf("basispoints returned a non-SSE response")
	}
	converted := prepared.Stream(ctx, resp.Body, func(summary basispoints.StreamSummary) {
		level := slog.LevelDebug
		if summary.DownstreamTerminal != "response.completed" || summary.ReadOrWriteError {
			level = slog.LevelWarn
		}
		logger := sdk.LoggerFromContext(ctx)
		if !logger.Enabled(ctx, level) {
			return
		}
		logger.Log(ctx, level, "basispoints_stream_finished",
			"model", req.Model, "account_id", req.Account.ID, "stream", fmt.Sprintf("%+v", summary))
	})
	if req.Stream {
		converted = newBasispointsKeepaliveBody(ctx, converted, basispointsKeepaliveInterval)
	}
	resp.Body = &basispointsBody{ReadCloser: converted, cancel: cancel}
	sdk.LoggerFromContext(ctx).Debug("basispoints_request_started", "model", req.Model, "account_id", req.Account.ID)
	return resp, true, nil
}

func basispointsHTTPFailure(resp *http.Response, start time.Time) sdk.ForwardOutcome {
	body, _ := readLimitedErrorBody(resp.Body)
	if !gjson.GetBytes(body, "error").IsObject() {
		body = openAIErrorJSON(openAIErrorTypeForStatus(resp.StatusCode), "", truncate(string(body), 200))
	}
	headers := http.Header{"Content-Type": {"application/json"}}
	if retry := resp.Header.Get("Retry-After"); retry != "" {
		headers.Set("Retry-After", retry)
	}
	outcome := failureOutcome(resp.StatusCode, body, headers, extractOpenAIErrorMessage(body), extractRetryAfterHeader(headers))
	if resp.StatusCode < 400 {
		outcome = transientOutcome("unexpected upstream redirect")
	}
	outcome.Duration = time.Since(start)
	return outcome
}

func (g *OpenAIGateway) tryBasispointsOAuth(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, bool, error) {
	if !basispointsEnabled(req) {
		return sdk.ForwardOutcome{}, false, nil
	}
	start := time.Now()
	body, err := basispointsResponsesBody(req)
	if err != nil {
		return sdk.ForwardOutcome{}, false, nil
	}
	body, _ = sjson.SetBytes(body, "model", req.Model)
	resp, used, err := g.openBasispoints(ctx, req, body)
	if !used {
		return sdk.ForwardOutcome{}, false, nil
	}
	if err != nil {
		return responseFailureOutcome(err, nil, false, time.Since(start)), true, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return basispointsHTTPFailure(resp, start), true, nil
	}

	var handler WSEventHandler
	var chat *chatCompletionsStreamWriter
	var sse *sseEventWriter
	timing := newResponseEventTiming(start)
	silent := &responsesSilentHandler{timing: timing}
	if req.Stream && req.Writer != nil {
		if isChatCompletionsRequest(req) {
			chat = newChatCompletionsStreamWriter(req.Writer, req.Model, 0, "", gjson.GetBytes(req.Body, "stream_options.include_usage").Bool(), start)
			handler = chat
		} else {
			sse = &sseEventWriter{w: req.Writer, timing: timing}
			sse.flusher, _ = req.Writer.(http.Flusher)
			handler = sse
		}
	} else {
		handler = silent
	}
	result := ParseSSEStream(resp.Body, handler, ctx)
	if chat != nil {
		timing = chat.timing
	} else if sse != nil {
		timing = sse.timing
	} else {
		timing = silent.timing
	}
	usage := newTokenUsage(firstNonEmptyString(result.Model, req.Model), result.ServiceTier, result.InputTokens, result.OutputTokens, result.CachedInputTokens, result.CacheCreationTokens, result.ReasoningOutputTokens, timing.firstEventMs)
	usage.FirstTokenMs = timing.firstTokenMs
	setUsageReasoningEffort(usage, gjson.GetBytes(body, "reasoning.effort").String())
	setUsageResponseID(usage, result.ResponseID)
	setUsageMetadata(usage, "oauth_transport", "basispoints")
	if result.StopReason == "max_output_tokens" {
		setUsageMetadata(usage, "openai.incomplete_reason", result.StopReason)
	}
	fillUsageCost(usage)
	if result.Err != nil {
		outputStarted := sse != nil && sse.wrote || chat != nil && chat.wrote
		outcome := responseFailureOutcome(result.Err, result.FailedEventRaw, outputStarted, time.Since(start))
		outcome.Usage = usage
		if outputStarted {
			if sse != nil {
				sse.writeTerminalErrorIfNeeded(http.StatusBadGateway, "basispoints_stream_incomplete", "Basispoints stream interrupted", result.ResponseID)
			}
			if chat != nil {
				_ = writeSSEError(req.Writer, "Basispoints stream interrupted")
			}
		}
		return outcome, true, nil
	}
	outcome := sdk.ForwardOutcome{Kind: sdk.OutcomeSuccess, Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK}, Usage: usage, Duration: time.Since(start)}
	switch {
	case chat != nil:
		chat.finalize()
		if chat.Err() != nil {
			outcome.Kind = sdk.OutcomeStreamAborted
			outcome.Reason = "client_write_failed"
		}
	case sse != nil:
		if result.StopReason == "max_output_tokens" {
			// The original response.incomplete was already forwarded. Codex treats
			// it as an unfinished response, even though HTTP transport succeeded.
			// Record the terminal failure without replaying, discarding usage, or
			// appending a second synthetic terminal event. Chat's length result
			// and non-streaming Responses' incomplete JSON remain valid results.
			outcome.Kind = sdk.OutcomeStreamAborted
			outcome.Reason = "response.incomplete: max_output_tokens"
			return outcome, true, nil
		}
		if err := writeSSEDone(req.Writer); err != nil {
			outcome.Kind = sdk.OutcomeStreamAborted
			outcome.Reason = "client_write_failed"
		}
	default:
		responseBody := buildNonStreamResponses(result)
		if isChatCompletionsRequest(req) {
			responseBody = buildNonStreamChatCompletion(result, req.Model)
		}
		outcome.Upstream.Headers = http.Header{"Content-Type": {"application/json"}}
		outcome.Upstream.Body = responseBody
	}
	return outcome, true, nil
}

// Native normalization strips controls that Codex ignores. Retain them for the
// BPS capability check, and map Chat's structured format before preparing BPS.
func basispointsResponsesBody(req *sdk.ForwardRequest) ([]byte, error) {
	body, err := wrapAsResponsesAPIWithTier(req.Body, req.Model, resolveOpenAIRequestServiceTier(req.Body, req.Headers))
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"previous_response_id", "temperature", "top_p", "max_output_tokens", "max_completion_tokens", "max_tokens", "truncation", "background", "context_management", "instructions"} {
		if value := gjson.GetBytes(req.Body, key); value.Exists() {
			body, _ = sjson.SetRawBytes(body, key, []byte(value.Raw))
		}
	}
	if format := gjson.GetBytes(req.Body, "response_format"); format.Exists() {
		if format.Get("type").String() == "json_schema" {
			body, _ = sjson.SetRawBytes(body, "text.format", []byte(format.Get("json_schema").Raw))
			body, _ = sjson.SetBytes(body, "text.format.type", "json_schema")
		} else {
			body, _ = sjson.SetRawBytes(body, "text.format", []byte(format.Raw))
		}
	}
	return body, nil
}

func (g *OpenAIGateway) tryBasispointsAnthropic(ctx context.Context, req *sdk.ForwardRequest, body []byte, originalModel, mappedModel string, start time.Time, w http.ResponseWriter) (sdk.ForwardOutcome, bool) {
	resp, used, err := g.openBasispoints(ctx, req, body)
	if !used {
		return sdk.ForwardOutcome{}, false
	}
	if err != nil {
		outcome := anthropicResponseFailureOutcome(err, false, time.Since(start))
		return outcome, true
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		outcome := basispointsHTTPFailure(resp, start)
		outcome.Upstream.Body = anthropicErrorJSON(anthropicErrorType(resp.StatusCode), extractOpenAIErrorMessage(outcome.Upstream.Body))
		return outcome, true
	}
	// BPS response IDs must not enter the native continuation/session cache.
	var outcome sdk.ForwardOutcome
	if req.Stream && w != nil {
		outcome, _ = translateResponsesSSEToAnthropicSSE(ctx, resp, w, originalModel, mappedModel, req.Body, "", "", start, g.streamIdleTimeout(), openAISessionResolution{})
	} else {
		outcome, _ = g.handleAnthropicNonStreamFromResponses(resp, nil, originalModel, mappedModel, req.Body, "", "", start, openAISessionResolution{}, 0)
	}
	setUsageReasoningEffort(outcome.Usage, gjson.GetBytes(body, "reasoning.effort").String())
	if outcome.Usage != nil {
		setUsageMetadata(outcome.Usage, "oauth_transport", "basispoints")
	}
	return outcome, true
}
