package gateway

import "github.com/tidwall/gjson"

// OAuth and API Key upstreams use the same policy. Ordinary requests remain
// ordinary. For priority (including fast) and flex requests, an explicit
// supported response tier determines billing; otherwise use the requested tier.
// Ordinary billing is represented by an empty Usage service_tier.
func resolveOpenAIUsageServiceTier(requestedTier, upstreamTier string) string {
	requested := normalizeOpenAIServiceTier(requestedTier)
	if requested == "" {
		return ""
	}
	if actual := normalizeOpenAIWireServiceTier(upstreamTier); actual != "" {
		return normalizeOpenAIServiceTier(actual)
	}
	return requested
}

func upstreamSSEServiceTier(eventType string, data []byte) (string, bool) {
	if isResponsesTerminalEvent(eventType) {
		// response.created may echo the requested tier. Only the terminal
		// response determines the actual tier, including an absent field.
		return gjson.GetBytes(data, "response.service_tier").String(), true
	}
	if eventType == "" || eventType == "chat.completion.chunk" {
		// Chat Completions may report the actual tier before the usage chunk.
		// Retain it when later chunks omit the field, but allow explicit downgrade.
		tier := gjson.GetBytes(data, "service_tier")
		return tier.String(), tier.Exists()
	}
	return "", false
}
