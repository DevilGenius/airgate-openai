package basispoints

import (
	"fmt"
	"net/url"
	"strings"
)

// AirGate always prepares inline images as BAS attachments in PrepareRequest.
// The wire converter accepts HTTPS URLs or validated attachment references;
// there is no separate image-enable or silently-ignore-images setting.
func validateImage(part object) error {
	if fileID, exists := part["file_id"]; exists {
		id, ok := fileID.(string)
		if !ok || !ValidAttachmentID(id) {
			return fmt.Errorf("basispoints input_image requires a valid file_id")
		}
		if _, exists := part["image_url"]; exists {
			return fmt.Errorf("basispoints input_image requires exactly one image reference")
		}
	} else {
		raw, ok := part["image_url"].(string)
		if !ok || raw == "" {
			return fmt.Errorf("basispoints input_image requires an HTTPS image_url or file_id")
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "data:") {
			return fmt.Errorf("inline images require BAS attachment preparation before wire conversion")
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || strings.TrimSpace(raw) != raw {
			return fmt.Errorf("basispoints input_image requires an absolute HTTPS image URL without embedded credentials")
		}
	}
	return validateImageDetail(part)
}

func validateImageDetail(part object) error {
	if detail, exists := part["detail"]; exists && detail != nil {
		switch text(detail) {
		case "auto", "low", "high", "original":
		default:
			return fmt.Errorf("basispoints image detail must be auto, low, high or original")
		}
	}
	return nil
}

func ValidAttachmentID(id string) bool {
	if !strings.HasPrefix(id, "file-") || len(id) < 6 || len(id) > 256 {
		return false
	}
	for _, c := range id[5:] {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}
