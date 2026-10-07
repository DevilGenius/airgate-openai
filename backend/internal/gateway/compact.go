package gateway

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const codexBetaFeaturesHeader = "X-Codex-Beta-Features"
const remoteCompactionV2Feature = "remote_compaction_v2"

// Compaction uses the normal Responses transport with a terminal input trigger.
func isResponsesCompactionRequest(path string, body []byte) bool {
	if !isResponsesRequestPath(path) {
		return false
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if item.Get("type").String() == "compaction_trigger" {
			return true
		}
	}
	return false
}

// Advertise compaction support for the whole Codex session, including replay.
// Preserve other client capabilities and avoid mutating the client's headers.
func ensureRemoteCompactionV2Header(headers http.Header) {
	if headers == nil {
		return
	}
	features := make([]string, 0, 4)
	seen := make(map[string]bool)
	for _, value := range headers.Values(codexBetaFeaturesHeader) {
		for _, feature := range strings.Split(value, ",") {
			feature = strings.TrimSpace(feature)
			if feature != "" && !seen[feature] {
				features = append(features, feature)
				seen[feature] = true
			}
		}
	}
	if !seen[remoteCompactionV2Feature] {
		features = append(features, remoteCompactionV2Feature)
	}
	headers.Set(codexBetaFeaturesHeader, strings.Join(features, ","))
}
