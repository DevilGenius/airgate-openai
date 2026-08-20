package gateway

import (
	"strings"
)

type responsesToolCallKey struct {
	itemType string
	callID   string
}

// normalizeResponsesToolCompatibilityFromMap is the tool-specific phase of the
// shared Responses request policy. Callers must enter through
// normalizeResponsesRequestMap instead of invoking this phase independently.
func normalizeResponsesToolCompatibilityFromMap(reqData map[string]any) bool {
	if reqData == nil {
		return false
	}

	changed := normalizeTopLevelResponsesTools(reqData)
	if normalizeResponsesInputToolItems(reqData) {
		changed = true
	}
	return changed
}

func normalizeTopLevelResponsesTools(reqData map[string]any) bool {
	rawTools, exists := reqData["tools"]
	if !exists {
		return false
	}

	tools, changed, valid := normalizeResponsesToolList(rawTools)
	if !valid {
		delete(reqData, "tools")
		delete(reqData, "tool_choice")
		delete(reqData, "parallel_tool_calls")
		return true
	}
	if !changed {
		return false
	}
	if len(tools) == 0 {
		delete(reqData, "tools")
		delete(reqData, "tool_choice")
		delete(reqData, "parallel_tool_calls")
		return true
	}
	reqData["tools"] = tools
	return true
}

func normalizeResponsesToolList(value any) (tools []any, changed bool, valid bool) {
	tools, valid = value.([]any)
	if !valid {
		return nil, true, false
	}

	writeIndex := 0
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			changed = true
			continue
		}
		keep, toolChanged := normalizeResponsesToolDefinition(tool)
		if toolChanged {
			changed = true
		}
		if !keep {
			changed = true
			continue
		}
		tools[writeIndex] = tool
		writeIndex++
	}
	if writeIndex != len(tools) {
		changed = true
	}
	return tools[:writeIndex], changed, true
}

func normalizeResponsesToolDefinition(tool map[string]any) (keep bool, changed bool) {
	rawToolType := jsonString(tool["type"])
	toolType := strings.TrimSpace(rawToolType)
	if toolType == "" {
		if !looksLikeResponsesFunctionTool(tool) {
			return false, true
		}
		tool["type"] = "function"
		toolType = "function"
		changed = true
	} else if toolType != rawToolType {
		tool["type"] = toolType
		changed = true
	}

	switch toolType {
	case "function":
		if flattenChatFunctionTool(tool) {
			changed = true
		}
		if strings.TrimSpace(jsonString(tool["name"])) == "" {
			return false, true
		}
	case "custom":
		if strings.TrimSpace(jsonString(tool["name"])) == "" {
			return false, true
		}
	case "namespace":
		if strings.TrimSpace(jsonString(tool["name"])) == "" {
			return false, true
		}
		children, childrenChanged, valid := normalizeResponsesToolList(tool["tools"])
		if !valid || len(children) == 0 {
			return false, true
		}
		if childrenChanged {
			tool["tools"] = children
			changed = true
		}
	}
	return true, changed
}

func looksLikeResponsesFunctionTool(tool map[string]any) bool {
	if fn, ok := tool["function"].(map[string]any); ok && strings.TrimSpace(jsonString(fn["name"])) != "" {
		return true
	}
	if strings.TrimSpace(jsonString(tool["name"])) == "" {
		return false
	}
	for _, field := range []string{"parameters", "parametersJsonSchema", "input_schema", "strict", "defer_loading"} {
		if _, ok := tool[field]; ok {
			return true
		}
	}
	return false
}

func flattenChatFunctionTool(tool map[string]any) bool {
	if strings.TrimSpace(jsonString(tool["name"])) != "" {
		return false
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		return false
	}
	name := strings.TrimSpace(jsonString(fn["name"]))
	if name == "" {
		return false
	}

	tool["name"] = name
	for _, field := range []string{"description", "parameters", "strict", "defer_loading"} {
		if _, exists := tool[field]; exists {
			continue
		}
		if value, exists := fn[field]; exists {
			tool[field] = value
		}
	}
	delete(tool, "function")
	return true
}

func normalizeResponsesInputToolItems(reqData map[string]any) bool {
	input, ok := reqData["input"].([]any)
	if !ok || len(input) == 0 {
		return false
	}

	allowOrphanOutputs := strings.TrimSpace(jsonString(reqData["previous_response_id"])) != ""
	var validCalls map[responsesToolCallKey]struct{}
	hasToolItems := false
	needsFilter := false
	changed := false

	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType := strings.TrimSpace(jsonString(item["type"]))

		if responsesInputItemCarriesTools(itemType) {
			hasToolItems = true
			tools, toolsChanged, valid := normalizeResponsesToolList(item["tools"])
			if !valid {
				if itemType == "additional_tools" {
					needsFilter = true
				} else {
					item["tools"] = []any{}
					changed = true
				}
			} else {
				if toolsChanged {
					item["tools"] = tools
					changed = true
				}
				if itemType == "additional_tools" && len(tools) == 0 {
					needsFilter = true
				}
			}
		}

		switch {
		case isOpenAIToolCallContextItemType(itemType):
			hasToolItems = true
			if !isValidResponsesToolCallContextItem(itemType, item) {
				if itemType == "function_call" || itemType == "custom_tool_call" {
					needsFilter = true
				}
				continue
			}
			if !allowOrphanOutputs {
				if validCalls == nil {
					validCalls = make(map[responsesToolCallKey]struct{})
				}
				validCalls[responsesToolCallKey{itemType: itemType, callID: strings.TrimSpace(jsonString(item["call_id"]))}] = struct{}{}
			}
		case isOpenAIToolOutputItemType(itemType):
			hasToolItems = true
			if !isServerToolSearchOutput(item) {
				callID := strings.TrimSpace(jsonString(item["call_id"]))
				if callID == "" || !allowOrphanOutputs {
					needsFilter = true
				}
			}
		}
	}

	if !hasToolItems || !needsFilter {
		return changed
	}

	writeIndex := 0
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			input[writeIndex] = rawItem
			writeIndex++
			continue
		}
		itemType := strings.TrimSpace(jsonString(item["type"]))
		if itemType == "additional_tools" {
			tools, valid := item["tools"].([]any)
			if !valid || len(tools) == 0 {
				changed = true
				continue
			}
		}
		if (itemType == "function_call" || itemType == "custom_tool_call") &&
			!isValidResponsesToolCallContextItem(itemType, item) {
			changed = true
			continue
		}
		if isOpenAIToolOutputItemType(itemType) && !isServerToolSearchOutput(item) {
			callID := strings.TrimSpace(jsonString(item["call_id"]))
			if callID == "" {
				changed = true
				continue
			}
			if !allowOrphanOutputs {
				callType := matchingToolCallTypeForOutputType(itemType)
				_, matched := validCalls[responsesToolCallKey{itemType: callType, callID: callID}]
				if callType == "" || !matched {
					changed = true
					continue
				}
			}
		}
		input[writeIndex] = rawItem
		writeIndex++
	}
	if writeIndex != len(input) {
		reqData["input"] = input[:writeIndex]
		changed = true
	}
	return changed
}

func responsesInputItemCarriesTools(itemType string) bool {
	switch itemType {
	case "additional_tools", "tool_search_output":
		return true
	default:
		return false
	}
}

func isValidResponsesToolCallContextItem(itemType string, item map[string]any) bool {
	if item == nil || strings.TrimSpace(jsonString(item["call_id"])) == "" {
		return false
	}
	switch strings.TrimSpace(itemType) {
	case "function_call":
		_, argumentsIsString := item["arguments"].(string)
		return strings.TrimSpace(jsonString(item["name"])) != "" && argumentsIsString
	case "custom_tool_call":
		_, hasInput := item["input"]
		return strings.TrimSpace(jsonString(item["name"])) != "" && hasInput
	default:
		return isOpenAIToolCallContextItemType(itemType)
	}
}

func isServerToolSearchOutput(item map[string]any) bool {
	return strings.TrimSpace(jsonString(item["type"])) == "tool_search_output" &&
		strings.EqualFold(strings.TrimSpace(jsonString(item["execution"])), "server")
}
