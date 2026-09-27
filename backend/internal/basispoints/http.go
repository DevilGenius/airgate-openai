package basispoints

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// ModelUnavailable permits one native attempt only after an explicit rejection.
// Authentication, safety, rate limits, timeouts and generic 5xx are not evidence
// that a model lacks BPS support and must never trigger an automatic replay.
func ModelUnavailable(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusForbidden && status != http.StatusNotFound {
		return false
	}
	code := strings.ToLower(gjson.GetBytes(body, "error.code").String())
	switch code {
	case "model_not_found", "model_not_supported", "unsupported_model", "model_access_denied", "basispoints_model_access_changed":
		return true
	}
	return false
}

// Headers is a fixed Excel client profile. Client-supplied credentials or profile
// headers are never copied. OAuth credentials continue to be owned by the gateway.
func Headers(authorization, accountID string) http.Header {
	h := make(http.Header)
	for key, value := range map[string]string{
		"Authorization":           authorization,
		"ChatGPT-Account-ID":      accountID,
		"X-OpenAI-Account-ID":     accountID,
		"X-Basispoints-Auth-Mode": "chatgpt",
		"Content-Type":            "application/json",
		"Accept":                  "text/event-stream",
		"Accept-Encoding":         "identity",
		"Origin":                  "https://bps.openai.com",
		"User-Agent":              "Mozilla/5.0",
		"X-OpenAI-Internal-Basispoints-Client-Agent-Profile":  "excel",
		"X-OpenAI-Internal-Basispoints-Client-Editor":         "excel",
		"X-OpenAI-Internal-Basispoints-Client-Host":           "office",
		"X-OpenAI-Internal-Basispoints-Client-Platform":       "excel",
		"X-OpenAI-Internal-Basispoints-Client-Platform-Class": "PC",
		"X-OpenAI-Internal-Basispoints-Client-Product":        "basispoints-excel-plugin",
		"X-OpenAI-Internal-Basispoints-Client-Runtime":        "desktop",
		"X-OpenAI-Internal-Basispoints-Office-Host":           "Excel",
		"X-OpenAI-Internal-Basispoints-Office-Platform":       "PC",
		"X-Stainless-Arch":            "unknown",
		"X-Stainless-Lang":            "js",
		"X-Stainless-OS":              "Unknown",
		"X-Stainless-Package-Version": "6.31.0",
		"X-Stainless-Retry-Count":     "0",
		"X-Stainless-Runtime":         "browser:chrome",
	} {
		h.Set(key, value)
	}
	return h
}
