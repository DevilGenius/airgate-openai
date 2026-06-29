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
			if strings.TrimSpace(jsonString(itemMap["call_id"])) != "" {
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
	case "function_call_output", "tool_search_output", "custom_tool_call_output", "mcp_tool_call_output":
		return true
	default:
		return false
	}
}

func requestNeedsPreviousResponseID(reqData map[string]any) bool {
	signals := analyzeToolContinuationSignalsFromMap(reqData)
	return signals.hasToolOutput && !signals.hasToolCallContext
}

func sanitizeUnmatchedFunctionCallOutputs(body []byte, allowPreviousContext bool) []byte {
	var reqData map[string]any
	if err := json.Unmarshal(body, &reqData); err != nil {
		return body
	}
	if !sanitizeUnmatchedFunctionCallOutputsFromMap(reqData, allowPreviousContext) {
		return body
	}
	patched, err := json.Marshal(reqData)
	if err != nil {
		return body
	}
	return patched
}

func sanitizeUnmatchedFunctionCallOutputsFromMap(reqData map[string]any, allowPreviousContext bool) bool {
	if reqData == nil {
		return false
	}
	if allowPreviousContext && strings.TrimSpace(jsonString(reqData["previous_response_id"])) != "" {
		return false
	}
	input, ok := reqData["input"].([]any)
	if !ok || len(input) == 0 {
		return false
	}

	callIDs := make(map[string]struct{})
	for _, item := range input {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(jsonString(itemMap["type"])) != "function_call" {
			continue
		}
		if callID := strings.TrimSpace(jsonString(itemMap["call_id"])); callID != "" {
			callIDs[callID] = struct{}{}
		}
	}

	filtered := make([]any, 0, len(input))
	changed := false
	for _, item := range input {
		itemMap, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if strings.TrimSpace(jsonString(itemMap["type"])) != "function_call_output" {
			filtered = append(filtered, item)
			continue
		}
		callID := strings.TrimSpace(jsonString(itemMap["call_id"]))
		if _, ok := callIDs[callID]; ok {
			filtered = append(filtered, item)
			continue
		}
		changed = true
	}
	if !changed {
		return false
	}
	reqData["input"] = filtered
	return true
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
	if sanitizeUnmatchedFunctionCallOutputsFromMap(reqData, false) {
		changed = true
	}
	if !changed || !responsesInputHasRecoverableContext(reqData) {
		return nil, false
	}
	patched, err := json.Marshal(reqData)
	if err != nil {
		return nil, false
	}
	return patched, true
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
		if strings.TrimSpace(jsonString(itemMap["type"])) != "function_call_output" {
			return true
		}
	}
	return false
}
