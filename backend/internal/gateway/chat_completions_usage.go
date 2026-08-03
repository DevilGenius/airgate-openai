package gateway

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// chatCompletionsStreamUsagePolicy keeps upstream billing requirements
// separate from the downstream client's stream contract. Every AirGate hop
// requests the final Chat Completions usage chunk from its upstream, but only
// the hop that injected the option hides that extra chunk from its client.
type chatCompletionsStreamUsagePolicy struct {
	upstreamBody        []byte
	hideUsageFromClient bool
}

func prepareChatCompletionsStreamUsage(
	reqPath string,
	stream bool,
	body []byte,
) chatCompletionsStreamUsagePolicy {
	policy := chatCompletionsStreamUsagePolicy{upstreamBody: body}
	if !stream || !isChatCompletionsPath(reqPath) || !json.Valid(body) {
		return policy
	}
	if gjson.GetBytes(body, "stream_options.include_usage").Bool() {
		return policy
	}

	patched, err := sjson.SetBytes(body, "stream_options.include_usage", true)
	if err != nil {
		return policy
	}
	policy.upstreamBody = patched
	policy.hideUsageFromClient = true
	return policy
}

func isChatCompletionsPath(path string) bool {
	path = strings.TrimSpace(path)
	if query := strings.IndexByte(path, '?'); query >= 0 {
		path = path[:query]
	}
	path = strings.TrimRight(path, "/")
	return strings.HasSuffix(strings.ToLower(path), "/chat/completions")
}

func isChatCompletionsUsageOnlyChunk(data []byte) bool {
	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() || usage.Type == gjson.Null {
		return false
	}
	choices := gjson.GetBytes(data, "choices")
	return choices.Exists() && choices.IsArray() && len(choices.Array()) == 0
}

func filterChatCompletionsUsageForClient(data []byte) (filtered []byte, suppress, changed bool) {
	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() || usage.Type == gjson.Null {
		return data, false, false
	}
	if isChatCompletionsUsageOnlyChunk(data) {
		return nil, true, false
	}
	filtered, err := sjson.DeleteBytes(data, "usage")
	if err != nil {
		return data, false, false
	}
	return filtered, false, true
}
