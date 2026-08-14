package gateway

import (
	"strings"

	"github.com/tidwall/gjson"
)

type explicitUpstreamError struct {
	UpstreamCode string
	Code         string
	Message      string
	Param        string
}

func parseExplicitUpstreamError(payload []byte) (explicitUpstreamError, bool) {
	return parseExplicitUpstreamErrorDepth(payload, 0)
}

func parseExplicitUpstreamErrorDepth(payload []byte, depth int) (explicitUpstreamError, bool) {
	if len(payload) == 0 || depth > 1 || !gjson.ValidBytes(payload) {
		return explicitUpstreamError{}, false
	}

	for _, path := range []string{"error", "response.error"} {
		node := gjson.GetBytes(payload, path)
		if !node.Exists() || !node.IsObject() {
			continue
		}
		upstreamCode := strings.ToLower(strings.TrimSpace(node.Get("code").String()))
		message := strings.TrimSpace(node.Get("message").String())
		if upstreamCode != "" {
			return explicitUpstreamError{
				UpstreamCode: upstreamCode,
				Code:         normalizeExplicitUpstreamErrorCode(upstreamCode),
				Message:      message,
				Param:        strings.TrimSpace(node.Get("param").String()),
			}, true
		}
		if nested := strings.TrimSpace(message); strings.HasPrefix(nested, "{") && gjson.Valid(nested) {
			if parsed, ok := parseExplicitUpstreamErrorDepth([]byte(nested), depth+1); ok {
				return parsed, true
			}
		}
	}
	return explicitUpstreamError{}, false
}

func normalizeExplicitUpstreamErrorCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "cyber_policy" {
		return cybersecurityRiskErrorCode
	}
	return code
}

func isExplicitSafetyRejectedCode(code string) bool {
	switch normalizeExplicitUpstreamErrorCode(code) {
	case cybersecurityRiskErrorCode,
		promptUsagePolicyErrorCode,
		safetyRejectionCode,
		"content_policy",
		"content_policy_violation",
		"policy_violation",
		"moderation_blocked":
		return true
	default:
		return false
	}
}

func isExplicitSafetyRejectedPayload(payload []byte) bool {
	rejection, ok := parseExplicitUpstreamError(payload)
	return ok && isExplicitSafetyRejectedCode(rejection.Code)
}

func isExplicitTextHashRejectionCode(code string) bool {
	switch normalizeExplicitUpstreamErrorCode(code) {
	case cybersecurityRiskErrorCode, promptUsagePolicyErrorCode, "invalid_encrypted_content":
		return true
	default:
		return false
	}
}
