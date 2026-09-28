package gateway

import "github.com/tidwall/gjson"

// Billing is independent of upstream transport and downstream protocol.
// A supported actual response tier wins; only an absent/unsupported response
// tier falls back to the effective request tier. Fast is an alias for priority.
// Ordinary billing is represented by an empty Usage service_tier.
func resolveOpenAIUsageServiceTier(requestedTier, upstreamTier string) string {
	if actual := normalizeOpenAIWireServiceTier(upstreamTier); actual != "" {
		return normalizeOpenAIServiceTier(actual)
	}
	return normalizeOpenAIServiceTier(requestedTier)
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
