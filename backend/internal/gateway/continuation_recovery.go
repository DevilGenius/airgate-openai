package gateway

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/DevilGenius/airgate-openai/backend/internal/model"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type previousResponseRecoverySignals struct {
	hasToolOutput       bool
	hasToolCallContext  bool
	hasEncryptedContent bool
	hasCompactionReplay bool
}

const (
	previousResponseRecoveryBytesPerToken = 6
	previousResponseRecoveryMinBodyBytes  = 512 << 10
	previousResponseRecoveryMaxBodyBytes  = 10 << 20
)

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
	return len(body) <= previousResponseRecoveryMaxBytesForBody(body, signals) &&
		!signals.hasEncryptedContent &&
		(!signals.hasToolOutput || signals.hasToolCallContext || signals.hasCompactionReplay)
}

func previousResponseRecoveryMaxBytesForBody(body []byte, signals previousResponseRecoverySignals) int {
	if signals.hasCompactionReplay {
		return previousResponseRecoveryMaxBodyBytes
	}
	contextWindow := model.Lookup(gjson.GetBytes(body, "model").String()).ContextWindow
	if contextWindow <= 0 {
		contextWindow = model.DefaultSpec.ContextWindow
	}
	limit := contextWindow * previousResponseRecoveryBytesPerToken
	if limit < previousResponseRecoveryMinBodyBytes {
		return previousResponseRecoveryMinBodyBytes
	}
	if limit > previousResponseRecoveryMaxBodyBytes {
		return previousResponseRecoveryMaxBodyBytes
	}
	return limit
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

func classifyOpenAIErrorBody(body []byte) *responsesFailureError {
	errNode := gjson.GetBytes(body, "error")
	if !errNode.Exists() {
		return nil
	}
	msg := strings.TrimSpace(errNode.Get("message").String())
	errType := strings.ToLower(strings.TrimSpace(errNode.Get("type").String()))
	errCode := strings.ToLower(strings.TrimSpace(errNode.Get("code").String()))
	if msg == "" && errType == "" && errCode == "" {
		return nil
	}
	return classifyResponsesError(errType, errCode, msg)
}
