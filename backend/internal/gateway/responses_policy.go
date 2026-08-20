package gateway

import (
	"net/http"
	"strings"
)

// responsesNormalizeOptions describes the only policy dimensions that affect
// input normalization. Keeping these here prevents HTTP, WebSocket, and
// continuation recovery paths from growing separate item-type switch trees.
type responsesNormalizeOptions struct {
	strictCodex bool
	finalize    bool
	model       string
	headers     http.Header
}

const codexResponsesLiteMetadataPath = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"

// normalizeResponsesRequestMap is the single map-level policy pipeline for
// Responses input and replay data. It intentionally does not touch status:
// status semantics are upstream/item-specific and are outside this policy.
func normalizeResponsesRequestMap(reqData map[string]any, opts responsesNormalizeOptions) bool {
	if reqData == nil {
		return false
	}

	changed := false
	lite := responsesLiteEnabled(reqData, opts)
	if normalizeResponsesLiteRequest(reqData, opts, lite) {
		changed = true
	}

	if input, ok := reqData["input"].([]any); ok {
		kept := make([]any, 0, len(input))
		instructions := make([]string, 0, 1)
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				kept = append(kept, raw)
				continue
			}

			itemType := strings.TrimSpace(jsonString(item["type"]))
			if opts.strictCodex && opts.finalize && codexReplayItemShouldDrop(reqData, itemType, item) {
				changed = true
				continue
			}

			role := strings.ToLower(strings.TrimSpace(jsonString(item["role"])))
			if role == "system" {
				if text := extractResponsesInputMessageTextFromMap(item); text != "" {
					instructions = append(instructions, text)
				}
				changed = true
				continue
			}

			if normalizeResponsesInputItemForPolicy(item, opts, lite) {
				changed = true
			}
			kept = append(kept, item)
		}
		if len(kept) != len(input) {
			reqData["input"] = kept
			changed = true
		}
		if len(instructions) > 0 {
			joined := strings.Join(instructions, "\n\n")
			if existing := strings.TrimSpace(jsonString(reqData["instructions"])); existing != "" {
				joined += "\n\n" + existing
			}
			if joined != jsonString(reqData["instructions"]) {
				reqData["instructions"] = joined
				changed = true
			}
		}
	} else if item, ok := reqData["input"].(map[string]any); ok {
		if normalizeResponsesInputItemForPolicy(item, opts, lite) {
			changed = true
		}
	}

	if opts.finalize && normalizeResponsesToolCompatibilityFromMap(reqData) {
		changed = true
	}
	return changed
}

func normalizeResponsesInputItemForPolicy(item map[string]any, opts responsesNormalizeOptions, lite bool) bool {
	if item == nil {
		return false
	}
	changed := false
	if opts.strictCodex {
		if _, exists := item["id"]; exists {
			delete(item, "id")
			changed = true
		}
	} else if normalizeResponsesInputItemID(item) {
		changed = true
	}
	if normalizeResponsesReasoningInputItem(item) {
		changed = true
	}

	itemType := strings.TrimSpace(jsonString(item["type"]))
	if opts.strictCodex && hasResponsesItemNamespace(itemType, item) {
		if lite {
			// Lite carries namespace as a first-class tool-call field.
		} else {
			delete(item, "namespace")
			changed = true
		}
	}
	return changed
}

func codexReplayItemShouldDrop(reqData map[string]any, itemType string, item map[string]any) bool {
	// A preserved upstream continuation owns its history. The local request
	// only contains the current turn, so server-output filtering would be wrong.
	if strings.TrimSpace(jsonString(reqData["previous_response_id"])) != "" {
		return false
	}

	// These outputs are server-owned artifacts, not reusable tool-call context.
	// Do not echo them into a stateless Codex replay; this is the same boundary
	// used by the replay cache and keeps image/search output from poisoning the
	// next request. status is deliberately not inspected or modified here.
	switch itemType {
	case "image_generation_call", "web_search_call":
		return true
	case "tool_search_output":
		return isServerToolSearchOutput(item)
	default:
		return false
	}
}

func hasResponsesItemNamespace(itemType string, item map[string]any) bool {
	if item == nil {
		return false
	}
	if _, exists := item["namespace"]; !exists {
		return false
	}
	switch itemType {
	case "function_call", "custom_tool_call", "mcp_tool_call", "tool_call", "local_shell_call", "tool_search_call":
		return true
	default:
		return false
	}
}

func extractResponsesInputMessageTextFromMap(item map[string]any) string {
	if item == nil {
		return ""
	}
	content := item["content"]
	switch value := content.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch strings.TrimSpace(jsonString(part["type"])) {
			case "input_text", "text", "output_text":
				if text := jsonString(part["text"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

func responsesLiteEnabled(reqData map[string]any, opts responsesNormalizeOptions) bool {
	if !opts.strictCodex {
		return false
	}
	if opts.headers != nil && strings.EqualFold(strings.TrimSpace(opts.headers.Get("x-openai-internal-codex-responses-lite")), "true") {
		return responsesLiteModelSupported(opts.modelOrBody(reqData))
	}
	if strings.EqualFold(strings.TrimSpace(jsonString(gjsonPathValue(reqData, codexResponsesLiteMetadataPath))), "true") {
		return responsesLiteModelSupported(opts.modelOrBody(reqData))
	}
	// Some clients lose the transport marker while retaining Lite-only
	// namespace fields. Recover the protocol mode only for known Lite models.
	if responsesLiteModelSupported(opts.modelOrBody(reqData)) {
		if input, ok := reqData["input"].([]any); ok {
			for _, raw := range input {
				item, ok := raw.(map[string]any)
				if ok && hasResponsesItemNamespace(strings.TrimSpace(jsonString(item["type"])), item) {
					return true
				}
			}
		}
	}
	return false
}

func (opts responsesNormalizeOptions) modelOrBody(reqData map[string]any) string {
	if strings.TrimSpace(opts.model) != "" {
		return opts.model
	}
	return jsonString(reqData["model"])
}

func gjsonPathValue(reqData map[string]any, path string) any {
	if reqData == nil {
		return nil
	}
	current := any(reqData)
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[segment]
	}
	return current
}

func normalizeResponsesLiteRequest(reqData map[string]any, opts responsesNormalizeOptions, enabled bool) bool {
	if !opts.strictCodex {
		return false
	}
	changed := false
	if enabled {
		metadata, _ := reqData["client_metadata"].(map[string]any)
		if metadata == nil {
			metadata = make(map[string]any)
			reqData["client_metadata"] = metadata
			changed = true
		}
		if jsonString(metadata["ws_request_header_x_openai_internal_codex_responses_lite"]) != "true" {
			metadata["ws_request_header_x_openai_internal_codex_responses_lite"] = "true"
			changed = true
		}
		if value, ok := reqData["parallel_tool_calls"]; !ok || value != false {
			reqData["parallel_tool_calls"] = false
			changed = true
		}
		reasoning, _ := reqData["reasoning"].(map[string]any)
		if reasoning == nil {
			reasoning = make(map[string]any)
			reqData["reasoning"] = reasoning
			changed = true
		}
		if jsonString(reasoning["context"]) != "all_turns" {
			reasoning["context"] = "all_turns"
			changed = true
		}
		return changed
	}

	if metadata, ok := reqData["client_metadata"].(map[string]any); ok {
		if _, exists := metadata["ws_request_header_x_openai_internal_codex_responses_lite"]; exists {
			delete(metadata, "ws_request_header_x_openai_internal_codex_responses_lite")
			changed = true
			if len(metadata) == 0 {
				delete(reqData, "client_metadata")
			}
		}
	}
	return changed
}

func responsesLiteModelSupported(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if slash := strings.LastIndexByte(id, '/'); slash >= 0 {
		id = id[slash+1:]
	}
	if strings.HasSuffix(id, "-openai-compact") {
		id = strings.TrimSuffix(id, "-openai-compact")
	}
	switch id {
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "codex-auto-review":
		return true
	default:
		return false
	}
}
