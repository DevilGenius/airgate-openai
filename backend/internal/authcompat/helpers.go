package authcompat

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/mail"
	"path/filepath"
	"strconv"
	"strings"
)

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}

func asMap(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return nil
}

func asSlice(value any) []any {
	if typed, ok := value.([]any); ok {
		return typed
	}
	return nil
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64, float32, int, int32, int64, uint, uint32, uint64, bool:
		return fmt.Sprint(typed)
	default:
		return ""
	}
}

func firstString(values ...any) string {
	for _, value := range values {
		if text := stringValue(value); text != "" && !strings.EqualFold(text, "unknown") {
			return text
		}
	}
	return ""
}

func nestedMap(source map[string]any, key string) map[string]any {
	if source == nil {
		return nil
	}
	return asMap(source[key])
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func mergeMissing(target map[string]any, source map[string]any) {
	for key, value := range source {
		if _, exists := target[key]; !exists {
			target[key] = value
		}
	}
}

func decodeJWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[1] == "" {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var payload map[string]any
	if err := decodeJSON(data, &payload); err != nil {
		return nil
	}
	return payload
}

func openAIAuth(payload map[string]any) map[string]any {
	return nestedMap(payload, "https://api.openai.com/auth")
}

func openAIProfile(payload map[string]any) map[string]any {
	return nestedMap(payload, "https://api.openai.com/profile")
}

func normalizeEmail(values ...any) string {
	for _, value := range values {
		candidate := strings.ToLower(strings.TrimSpace(stringValue(value)))
		if candidate == "" {
			continue
		}
		parsed, err := mail.ParseAddress(candidate)
		if err == nil && parsed.Address == candidate {
			return candidate
		}
	}
	return ""
}

func fallbackName(fileName string) string {
	name := filepath.Base(strings.TrimSpace(fileName))
	for _, suffix := range []string{".auth.json", ".json"} {
		if strings.HasSuffix(strings.ToLower(name), suffix) {
			name = name[:len(name)-len(suffix)]
			break
		}
	}
	if strings.TrimSpace(name) == "" {
		return "OpenAI OAuth"
	}
	return name
}

func intValue(value any, fallback int) int {
	text := stringValue(value)
	if text == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return fallback
	}
	return int(parsed)
}

func floatValue(value any, fallback float64) float64 {
	text := stringValue(value)
	if text == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func setCredential(target map[string]string, key string, values ...any) {
	if _, exists := target[key]; exists {
		return
	}
	if value := firstString(values...); value != "" {
		target[key] = value
	}
}

func copyCredentialFields(target map[string]string, sources []map[string]any) {
	aliases := map[string][]string{
		"access_token":               {"access_token", "accessToken"},
		"refresh_token":              {"refresh_token", "refreshToken"},
		"id_token":                   {"id_token", "idToken"},
		"session_token":              {"session_token", "sessionToken"},
		"api_key":                    {"api_key", "apiKey", "OPENAI_API_KEY"},
		"base_url":                   {"base_url", "baseUrl"},
		"account_id":                 {"account_id", "accountId"},
		"account_name":               {"account_name", "accountName"},
		"chatgpt_account_id":         {"chatgpt_account_id", "chatgptAccountId"},
		"chatgpt_user_id":            {"chatgpt_user_id", "chatgptUserId", "user_id", "userId"},
		"chatgpt_account_is_fedramp": {"chatgpt_account_is_fedramp", "chatgptAccountIsFedramp"},
		"client_id":                  {"client_id", "clientId"},
		"email":                      {"email", "user_email", "userEmail"},
		"expires_at":                 {"expires_at", "expiresAt", "expired", "expires"},
		"organization_id":            {"organization_id", "organizationId"},
		"plan_type":                  {"plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType"},
		"privacy_mode":               {"privacy_mode", "privacyMode"},
		"provider":                   {"provider"},
		"subscription_active_until":  {"subscription_active_until", "subscriptionActiveUntil", "subscription_expires_at", "subscriptionExpiresAt"},
		"workspace_id":               {"workspace_id", "workspaceId"},
		"auth_mode":                  {"auth_mode", "authMode"},
		"agent_runtime_id":           {"agent_runtime_id", "agentRuntimeId"},
		"agent_private_key":          {"agent_private_key", "agentPrivateKey"},
		"task_id":                    {"task_id", "taskId"},
	}
	for canonical, keys := range aliases {
		for _, source := range sources {
			values := make([]any, 0, len(keys))
			for _, key := range keys {
				values = append(values, source[key])
			}
			if value := firstString(values...); value != "" {
				setCredential(target, canonical, value)
				break
			}
		}
	}
}
