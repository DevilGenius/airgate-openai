package gateway

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func enforceOperationPolicies(req *sdk.ForwardRequest, reqPath string) (sdk.ForwardOutcome, bool) {
	if req == nil {
		return sdk.ForwardOutcome{}, false
	}
	switch {
	case isImagesGenerationsPath(reqPath):
		if !operationEnabled(req.Headers, "images.generate") {
			return operationDeniedOutcome("images_generate_disabled", "Image generation is not enabled for this group"), true
		}
		return sdk.ForwardOutcome{}, false
	case isImagesEditsPath(reqPath):
		if !operationEnabled(req.Headers, "images.edit") {
			return operationDeniedOutcome("images_edit_disabled", "Image generation is not enabled for this group"), true
		}
		return sdk.ForwardOutcome{}, false
	}
	if hasResponsesImageGenerationTool(req.Body) && !operationEnabled(req.Headers, "responses.image_generation") {
		req.Body = removeResponsesImageGenerationTool(req.Body)
	}
	return sdk.ForwardOutcome{}, false
}

func operationEnabled(headers http.Header, operation string) bool {
	if headers == nil || strings.TrimSpace(operation) == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(headers.Get("X-Airgate-Operation-"+canonicalOperationHeader(operation))), "true")
}

func canonicalOperationHeader(operation string) string {
	parts := strings.FieldsFunc(operation, func(r rune) bool {
		return r == '.' || r == '_' || r == '-'
	})
	for i, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, "-")
}

func hasResponsesImageGenerationTool(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	for _, item := range gjson.GetBytes(body, "tools").Array() {
		if strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "image_generation") {
			return true
		}
	}
	return false
}

func removeResponsesImageGenerationTool(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	items := tools.Array()
	if len(items) == 0 {
		return body
	}

	updated := body
	removed := false
	for i := len(items) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(items[i].Get("type").String()), "image_generation") {
			continue
		}
		next, err := sjson.DeleteBytes(updated, fmt.Sprintf("tools.%d", i))
		if err != nil {
			continue
		}
		updated = next
		removed = true
	}
	if !removed {
		return body
	}

	if len(gjson.GetBytes(updated, "tools").Array()) == 0 {
		if next, err := sjson.DeleteBytes(updated, "tools"); err == nil {
			updated = next
		}
	}
	if isImageGenerationToolChoice(gjson.GetBytes(updated, "tool_choice")) {
		if next, err := sjson.DeleteBytes(updated, "tool_choice"); err == nil {
			updated = next
		}
	}
	return updated
}

func isImageGenerationToolChoice(choice gjson.Result) bool {
	switch {
	case !choice.Exists():
		return false
	case choice.IsObject():
		return strings.EqualFold(strings.TrimSpace(choice.Get("type").String()), "image_generation")
	default:
		return strings.EqualFold(strings.TrimSpace(choice.String()), "image_generation")
	}
}

func isImagesGenerationsPath(reqPath string) bool {
	path := normalizeForwardedPath(reqPath)
	return path == "/v1/images/generations" || path == "/images/generations"
}

func isImagesEditsPath(reqPath string) bool {
	path := normalizeForwardedPath(reqPath)
	return path == "/v1/images/edits" || path == "/images/edits"
}

func normalizeForwardedPath(reqPath string) string {
	reqPath = strings.TrimSpace(strings.ToLower(reqPath))
	if reqPath == "" {
		return ""
	}
	if idx := strings.Index(reqPath, "?"); idx >= 0 {
		reqPath = reqPath[:idx]
	}
	if strings.HasSuffix(reqPath, "/") && len(reqPath) > 1 {
		reqPath = strings.TrimRight(reqPath, "/")
	}
	return reqPath
}

func operationDeniedOutcome(code, message string) sdk.ForwardOutcome {
	body := openAIErrorJSON("invalid_request_error", code, message)
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusForbidden,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		},
		Reason: message,
	}
}
