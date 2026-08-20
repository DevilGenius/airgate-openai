package gateway

import (
	"encoding/json"
	"strings"
)

type toolContinuationSignals struct {
	hasToolOutput      bool
	hasToolCallContext bool
}

func analyzeToolContinuationSignalsFromMap(reqData map[string]any) toolContinuationSignals {
	var signals toolContinuationSignals
	if reqData == nil {
		return signals
	}
	input, ok := reqData["input"].([]any)
	if !ok {
		return signals
	}
	for _, item := range input {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := itemMap["type"].(string)
		switch {
		case isOpenAIToolOutputItemType(itemType):
			signals.hasToolOutput = true
		case isOpenAIToolCallContextItemType(itemType):
			if isValidResponsesToolCallContextItem(itemType, itemMap) {
				signals.hasToolCallContext = true
			}
		}
	}
	return signals
}

func isOpenAIToolCallContextItemType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "tool_call", "function_call", "local_shell_call", "tool_search_call", "custom_tool_call", "mcp_tool_call":
		return true
	default:
		return false
	}
}

func isOpenAIToolOutputItemType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "function_call_output", "local_shell_call_output", "tool_search_output", "custom_tool_call_output", "mcp_tool_call_output":
		return true
	default:
		return false
	}
}

func matchingToolCallTypeForOutputType(itemType string) string {
	switch strings.TrimSpace(itemType) {
	case "function_call_output":
		return "function_call"
	case "custom_tool_call_output":
		return "custom_tool_call"
	case "local_shell_call_output":
		return "local_shell_call"
	case "tool_search_output":
		return "tool_search_call"
	case "mcp_tool_call_output":
		return "mcp_tool_call"
	default:
		return ""
	}
}

func requestNeedsPreviousResponseID(reqData map[string]any) bool {
	signals := analyzeToolContinuationSignalsFromMap(reqData)
	return signals.hasToolOutput && !signals.hasToolCallContext
}

func functionCallOutputRecoveryBody(body []byte) ([]byte, bool) {
	var reqData map[string]any
	if err := json.Unmarshal(body, &reqData); err != nil {
		return nil, false
	}

	changed := false
	if _, ok := reqData["previous_response_id"]; ok {
		delete(reqData, "previous_response_id")
		changed = true
	}
	if normalizeResponsesRequestMap(reqData, responsesNormalizeOptions{finalize: true}) {
		changed = true
	}
	if sanitizeEncryptedReasoningItems(reqData) {
		changed = true
	}
	if !changed || !responsesInputHasRecoverableContext(reqData) {
		return nil, false
	}
	patched, err := json.Marshal(reqData)
	if err != nil {
		return nil, false
	}
	return normalizeResponsesInput(patched, "/v1/responses"), true
}

func responsesInputHasRecoverableContext(reqData map[string]any) bool {
	input, ok := reqData["input"].([]any)
	if !ok {
		return true
	}
	for _, item := range input {
		itemMap, ok := item.(map[string]any)
		if !ok {
			return true
		}
		if !isOpenAIToolOutputItemType(strings.TrimSpace(jsonString(itemMap["type"]))) {
			return true
		}
	}
	return false
}
