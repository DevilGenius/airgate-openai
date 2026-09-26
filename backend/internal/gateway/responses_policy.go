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
	headers     http.Header
}

const codexResponsesLiteMetadataPath = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"

const codexResponsesLiteHeader = "X-Openai-Internal-Codex-Responses-Lite"

func responsesLiteMarkerEnabled(value any) bool {
	switch value := value.(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "true")
	default:
		return false
	}
}

// Protocol headers must survive authentication refresh independently of credentials.
func passResponsesProtocolHeaders(src, dst http.Header) {
	if values := src.Values(codexResponsesLiteHeader); len(values) > 0 && dst != nil {
		dst[codexResponsesLiteHeader] = append([]string(nil), values...)
	}
}

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

			if normalizeResponsesInputItemForPolicy(item, opts) {
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
		if normalizeResponsesInputItemForPolicy(item, opts) {
			changed = true
		}
	}

	if opts.finalize && normalizeResponsesToolCompatibilityFromMap(reqData) {
		changed = true
	}
	return changed
}

func normalizeResponsesInputItemForPolicy(item map[string]any, opts responsesNormalizeOptions) bool {
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
		return true
	}
	if responsesLiteMarkerEnabled(gjsonPathValue(reqData, codexResponsesLiteMetadataPath)) {
		return true
	}
	// Namespaces are also valid in ordinary Responses tool history. Only an
	// explicit transport marker may opt a request into the Lite dialect; model
	// capabilities and tool shapes must never activate it implicitly.
	return false
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

	return changed
}
