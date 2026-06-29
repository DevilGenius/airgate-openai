package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const (
	airgateContinuationRecoveryHeader       = "X-Airgate-Continuation-Recovery"
	airgateContinuationRecoveryDropPrevious = "drop_previous_response_id"
	airgateContinuationRecoveryApplied      = "X-Airgate-Continuation-Recovery-Applied"
)

type previousResponseRecoverySignals struct {
	hasToolOutput       bool
	hasToolCallContext  bool
	hasEncryptedContent bool
	hasCompactionReplay bool
}

func applyAirgateContinuationRecovery(body []byte, headers http.Header) []byte {
	if headers != nil {
		headers.Del(airgateContinuationRecoveryApplied)
	}
	if !airgateContinuationRecoveryRequested(headers) {
		return body
	}
	hadPreviousResponseID := gjson.GetBytes(body, "previous_response_id").Exists()
	patched, ok := delegatedContinuationRecoveryBody(body)
	if !ok {
		return body
	}
	removedPreviousResponseID := hadPreviousResponseID && !gjson.GetBytes(patched, "previous_response_id").Exists()
	if headers != nil && removedPreviousResponseID && !bytes.Equal(bytes.TrimSpace(patched), bytes.TrimSpace(body)) {
		headers.Set(airgateContinuationRecoveryApplied, "true")
	}
	return patched
}

func airgateContinuationRecoveryRequested(headers http.Header) bool {
	if headers == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(headers.Get(airgateContinuationRecoveryHeader)), airgateContinuationRecoveryDropPrevious)
}

func delegatedContinuationRecoveryBody(body []byte) ([]byte, bool) {
	var reqData map[string]any
	if err := json.Unmarshal(body, &reqData); err != nil {
		return nil, false
	}
	signals := analyzePreviousResponseRecoverySignals(reqData)
	if signals.hasToolOutput && !signals.hasToolCallContext && !signals.hasCompactionReplay {
		return nil, false
	}

	changed := false
	if _, ok := reqData["previous_response_id"]; ok {
		delete(reqData, "previous_response_id")
		changed = true
	}
	if sanitizeEncryptedReasoningItems(reqData) {
		changed = true
	}
	if !changed {
		return body, true
	}

	patched, err := json.Marshal(reqData)
	if err != nil {
		return nil, false
	}
	return patched, true
}

func delegatedContinuationRecoveryApplied(body []byte, headers http.Header) bool {
	if !airgateContinuationRecoveryRequested(headers) || gjson.GetBytes(body, "previous_response_id").Exists() {
		return false
	}
	if headers == nil || !strings.EqualFold(strings.TrimSpace(headers.Get(airgateContinuationRecoveryApplied)), "true") {
		return false
	}
	var reqData map[string]any
	if err := json.Unmarshal(body, &reqData); err != nil {
		return false
	}
	signals := analyzePreviousResponseRecoverySignals(reqData)
	return !signals.hasToolOutput || signals.hasToolCallContext || signals.hasCompactionReplay
}

func previousResponseNotFoundRecoveryBody(body []byte) ([]byte, bool) {
	if !requestCanRecoverPreviousResponseNotFound(body) {
		return nil, false
	}
	patched, err := sjson.DeleteBytes(body, "previous_response_id")
	if err != nil {
		return nil, false
	}
	return patched, true
}

func requestCanRecoverPreviousResponseNotFound(body []byte) bool {
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) == "" {
		return false
	}

	var reqData map[string]any
	if err := json.Unmarshal(body, &reqData); err != nil {
		return false
	}
	signals := analyzePreviousResponseRecoverySignals(reqData)
	return !signals.hasEncryptedContent &&
		(!signals.hasToolOutput || signals.hasToolCallContext || signals.hasCompactionReplay)
}

func sanitizeEncryptedReasoningItems(reqData map[string]any) bool {
	if len(reqData) == 0 {
		return false
	}
	changed := false
	if input, ok := reqData["input"]; ok {
		next, inputChanged, keep := sanitizeEncryptedReasoningValue(input)
		if inputChanged {
			changed = true
			if keep {
				reqData["input"] = next
			} else {
				delete(reqData, "input")
			}
		}
	}
	if messages, ok := reqData["messages"]; ok {
		next, messagesChanged, keep := sanitizeEncryptedReasoningValue(messages)
		if messagesChanged {
			changed = true
			if keep {
				reqData["messages"] = next
			} else {
				delete(reqData, "messages")
			}
		}
	}
	return changed
}

func sanitizeEncryptedReasoningValue(value any) (next any, changed bool, keep bool) {
	switch v := value.(type) {
	case []any:
		filtered := v[:0]
		changed := false
		for _, item := range v {
			nextItem, itemChanged, keepItem := sanitizeEncryptedReasoningItem(item)
			if itemChanged {
				changed = true
			}
			if keepItem {
				filtered = append(filtered, nextItem)
			}
		}
		if !changed {
			return value, false, true
		}
		if len(filtered) == 0 {
			return nil, true, false
		}
		return filtered, true, true
	case map[string]any:
		return sanitizeEncryptedReasoningItem(v)
	default:
		return value, false, true
	}
}

func sanitizeEncryptedReasoningItem(item any) (next any, changed bool, keep bool) {
	itemMap, ok := item.(map[string]any)
	if !ok {
		return item, false, true
	}
	if strings.TrimSpace(jsonString(itemMap["type"])) != "reasoning" {
		return item, false, true
	}
	if strings.TrimSpace(jsonString(itemMap["encrypted_content"])) == "" {
		return item, false, true
	}

	return nil, true, false
}

func analyzePreviousResponseRecoverySignals(reqData map[string]any) previousResponseRecoverySignals {
	var signals previousResponseRecoverySignals
	if reqData == nil {
		return signals
	}
	analyzePreviousResponseInputSignals(reqData["input"], &signals)
	analyzePreviousResponseMessagesSignals(reqData["messages"], &signals)
	return signals
}

func analyzePreviousResponseInputSignals(input any, signals *previousResponseRecoverySignals) {
	if signals == nil {
		return
	}
	switch v := input.(type) {
	case []any:
		for _, item := range v {
			itemMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			analyzePreviousResponseInputItemSignals(itemMap, signals)
		}
	case map[string]any:
		analyzePreviousResponseInputItemSignals(v, signals)
	}
}

func analyzePreviousResponseInputItemSignals(item map[string]any, signals *previousResponseRecoverySignals) {
	itemType := strings.TrimSpace(jsonString(item["type"]))
	if isCompactionReplayItemType(itemType) {
		signals.hasCompactionReplay = true
	}
	if itemType == "reasoning" && strings.TrimSpace(jsonString(item["encrypted_content"])) != "" {
		signals.hasEncryptedContent = true
	}
	switch {
	case isOpenAIToolOutputItemType(itemType):
		signals.hasToolOutput = true
	case isOpenAIToolCallContextItemType(itemType):
		if strings.TrimSpace(jsonString(item["call_id"])) != "" {
			signals.hasToolCallContext = true
		}
	}
}

func analyzePreviousResponseMessagesSignals(messages any, signals *previousResponseRecoverySignals) {
	if signals == nil {
		return
	}
	items, ok := messages.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(jsonString(msg["role"])))
		msgType := strings.TrimSpace(jsonString(msg["type"]))
		if msgType == "reasoning" && strings.TrimSpace(jsonString(msg["encrypted_content"])) != "" {
			signals.hasEncryptedContent = true
		}
		if role == "tool" {
			signals.hasToolOutput = true
		}
		if role == "assistant" {
			if _, ok := msg["tool_calls"]; ok {
				signals.hasToolCallContext = true
			}
			if _, ok := msg["function_call"]; ok {
				signals.hasToolCallContext = true
			}
		}
		analyzePreviousResponseMessageContentSignals(msg["content"], signals)
	}
}

func analyzePreviousResponseMessageContentSignals(content any, signals *previousResponseRecoverySignals) {
	switch v := content.(type) {
	case []any:
		for _, item := range v {
			itemMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			analyzePreviousResponseMessageContentItemSignals(itemMap, signals)
		}
	case map[string]any:
		analyzePreviousResponseMessageContentItemSignals(v, signals)
	}
}

func analyzePreviousResponseMessageContentItemSignals(item map[string]any, signals *previousResponseRecoverySignals) {
	itemType := strings.TrimSpace(jsonString(item["type"]))
	switch itemType {
	case "reasoning":
		if strings.TrimSpace(jsonString(item["encrypted_content"])) != "" {
			signals.hasEncryptedContent = true
		}
	case "compaction", "compaction_summary":
		signals.hasCompactionReplay = true
	case "tool_result":
		signals.hasToolOutput = true
	case "tool_use":
		signals.hasToolCallContext = true
	}
}

func isCompactionReplayItemType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "compaction", "compaction_summary":
		return true
	default:
		return false
	}
}

func isPreviousResponseNotFoundFailure(failure *responsesFailureError) bool {
	return failure != nil &&
		failure.Kind == responsesFailureKindContinuationAnchor &&
		strings.TrimSpace(failure.Code) == "previous_response_not_found"
}

func isPreviousResponseNotFoundError(err error) bool {
	var failure *responsesFailureError
	return errors.As(err, &failure) && isPreviousResponseNotFoundFailure(failure)
}

func isFunctionCallOutputWithoutCallFailure(failure *responsesFailureError) bool {
	return failure != nil &&
		failure.Kind == responsesFailureKindContinuationAnchor &&
		strings.TrimSpace(failure.Code) == "function_call_output_without_call"
}

func isFunctionCallOutputWithoutCallErrorResult(err error) bool {
	var failure *responsesFailureError
	return errors.As(err, &failure) && isFunctionCallOutputWithoutCallFailure(failure)
}

func isContextTooLargeFailure(failure *responsesFailureError) bool {
	return failure != nil &&
		failure.Kind == responsesFailureKindClient &&
		strings.TrimSpace(failure.Code) == "context_too_large"
}

func isContextTooLargeErrorResult(err error) bool {
	if err == nil {
		return false
	}
	var failure *responsesFailureError
	if errors.As(err, &failure) {
		return isContextTooLargeFailure(failure)
	}
	return isContextTooLargeError(err.Error())
}

func outcomeIsPreviousResponseNotFound(outcome sdk.ForwardOutcome) bool {
	if outcome.Kind != sdk.OutcomeClientError && outcome.Upstream.StatusCode < 400 {
		return false
	}
	if failure := classifyOpenAIErrorBody(outcome.Upstream.Body); isPreviousResponseNotFoundFailure(failure) {
		return true
	}
	if reason := strings.TrimSpace(outcome.Reason); reason != "" {
		return isPreviousResponseNotFoundFailure(classifyResponsesError("", "", reason))
	}
	return false
}

func outcomeIsContextTooLarge(outcome sdk.ForwardOutcome) bool {
	if outcome.Kind != sdk.OutcomeClientError && outcome.Upstream.StatusCode < 400 {
		return false
	}
	if failure := classifyOpenAIErrorBody(outcome.Upstream.Body); isContextTooLargeFailure(failure) {
		return true
	}
	if reason := strings.TrimSpace(outcome.Reason); reason != "" {
		if isContextTooLargeFailure(classifyResponsesError("", "", reason)) {
			return true
		}
		return isContextTooLargeError(reason)
	}
	return false
}

func responsesCompressionFallbackBody(body []byte, currentModel string) ([]byte, string, bool) {
	fallback := strings.TrimSpace(responsesCompressionFallbackModel())
	if fallback == "" || mappedModelIDsEqual(currentModel, fallback) {
		return nil, "", false
	}
	patched, err := sjson.SetBytes(body, "model", fallback)
	if err != nil {
		return nil, "", false
	}
	return patched, fallback, true
}

func mappedModelIDsEqual(a, b string) bool {
	left := normalizeMappedModelID(a, "")
	right := normalizeMappedModelID(b, "")
	return left != "" && right != "" && strings.EqualFold(left, right)
}

func classifyOpenAIErrorBody(body []byte) *responsesFailureError {
	errNode := gjson.GetBytes(body, "error")
	if !errNode.Exists() {
		msg := strings.TrimSpace(gjson.GetBytes(body, "detail").String())
		if msg == "" {
			msg = strings.TrimSpace(gjson.GetBytes(body, "message").String())
		}
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "code").String()))
		errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "type").String()))
		if msg == "" && errType == "" && code == "" {
			return nil
		}
		return classifyResponsesError(errType, code, msg)
	}
	msg := strings.TrimSpace(errNode.Get("message").String())
	errType := strings.ToLower(strings.TrimSpace(errNode.Get("type").String()))
	errCode := strings.ToLower(strings.TrimSpace(errNode.Get("code").String()))
	if msg == "" && errType == "" && errCode == "" {
		return nil
	}
	return classifyResponsesError(errType, errCode, msg)
}
