package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strings"
	"unicode/utf8"
)

const finalErrorImageRedactionReason = "image_input"

type finalErrorDiagnosticBodySnapshot struct {
	Body            []byte
	ContentType     string
	Redacted        bool
	RedactionReason string
	OriginalSize    int64
}

func sanitizeFinalErrorDiagnosticBody(body []byte, contentType string, forceImageRequest bool) finalErrorDiagnosticBodySnapshot {
	snapshot := finalErrorDiagnosticBodySnapshot{Body: body, ContentType: contentType}
	if len(body) == 0 {
		return snapshot
	}

	mediaType, _, _ := mime.ParseMediaType(contentType)
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return redactedFinalErrorBody(nil, len(body))
	case strings.HasPrefix(mediaType, "multipart/"):
		redacted, changed, err := redactFinalErrorMultipartBody(body, contentType, forceImageRequest)
		if err != nil {
			if forceImageRequest || finalErrorBodyMayContainImageInput(body) {
				return redactedFinalErrorBody(nil, len(body))
			}
			return snapshot
		}
		if changed {
			return redactedFinalErrorBody(redacted, len(body))
		}
		return snapshot
	case isFinalErrorJSONMediaType(mediaType) || json.Valid(body):
		redacted, changed, err := redactFinalErrorJSONBody(body)
		if err != nil {
			if forceImageRequest || finalErrorBodyMayContainImageInput(body) {
				return redactedFinalErrorBody(nil, len(body))
			}
			return snapshot
		}
		if changed {
			return redactedFinalErrorBody(redacted, len(body))
		}
		return snapshot
	case forceImageRequest:
		return redactedFinalErrorBody(nil, len(body))
	default:
		return snapshot
	}
}

func redactedFinalErrorBody(body []byte, originalSize int) finalErrorDiagnosticBodySnapshot {
	return finalErrorDiagnosticBodySnapshot{
		Body:            body,
		ContentType:     "application/json",
		Redacted:        true,
		RedactionReason: finalErrorImageRedactionReason,
		OriginalSize:    int64(originalSize),
	}
}

func redactFinalErrorJSONBody(body []byte) ([]byte, bool, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, false, errors.New("request body contains trailing JSON data")
	}
	redacted, keep, changed := redactFinalErrorJSONValue(value)
	if !keep {
		redacted = map[string]any{}
		changed = true
	}
	if !changed {
		return body, false, nil
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return nil, false, err
	}
	return encoded, true, nil
}

func redactFinalErrorJSONValue(value any) (any, bool, bool) {
	switch current := value.(type) {
	case []any:
		out := make([]any, 0, len(current))
		changed := false
		for _, item := range current {
			redacted, keep, itemChanged := redactFinalErrorJSONValue(item)
			changed = changed || itemChanged
			if !keep {
				changed = true
				continue
			}
			out = append(out, redacted)
		}
		return out, true, changed
	case map[string]any:
		if isFinalErrorImageContentBlock(current) {
			return nil, false, true
		}
		out := make(map[string]any, len(current))
		changed := false
		for key, item := range current {
			if isFinalErrorImageField(key) {
				changed = true
				continue
			}
			redacted, keep, itemChanged := redactFinalErrorJSONValue(item)
			changed = changed || itemChanged
			if !keep {
				changed = true
				continue
			}
			out[key] = redacted
		}
		return out, true, changed
	case string:
		if isFinalErrorInlineImage(current) {
			return nil, false, true
		}
	}
	return value, true, false
}

func isFinalErrorImageContentBlock(value map[string]any) bool {
	itemType, _ := value["type"].(string)
	switch strings.ToLower(strings.TrimSpace(itemType)) {
	case "image", "image_url", "input_image", "input_image_url", "image_file", "input_image_file", "computer_screenshot", "screenshot":
		return true
	}
	mediaType, _ := value["media_type"].(string)
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "image/") {
		return true
	}
	mimeType, _ := value["mime_type"].(string)
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/")
}

func isFinalErrorImageField(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "image", "images", "mask", "masks",
		"image_url", "image_urls", "input_image", "input_images",
		"input_image_url", "input_image_urls", "image_data", "image_base64",
		"input_image_data", "reference_image", "reference_images",
		"source_image", "source_images", "init_image", "init_images":
		return true
	default:
		return false
	}
}

func isFinalErrorInlineImage(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "data:image/")
}

func finalErrorBodyMayContainImageInput(body []byte) bool {
	for _, marker := range [][]byte{
		[]byte("data:image/"),
		[]byte(`"image"`),
		[]byte(`"images"`),
		[]byte(`"mask"`),
		[]byte(`"image_url"`),
		[]byte(`"input_image"`),
		[]byte("image/"),
		[]byte(".png\""),
		[]byte(".jpg\""),
		[]byte(".jpeg\""),
		[]byte(".webp\""),
		[]byte(".gif\""),
	} {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

func redactFinalErrorMultipartBody(body []byte, contentType string, canonicalize bool) ([]byte, bool, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, false, err
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, false, errors.New("multipart content type is missing boundary")
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	fields := make(map[string][]string)
	changed := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, err
		}
		data, readErr := io.ReadAll(part)
		name := part.FormName()
		filename := part.FileName()
		partContentType := strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Type")))
		_ = part.Close()
		if readErr != nil {
			return nil, false, readErr
		}
		drop := filename != "" || isFinalErrorImageField(name) ||
			strings.HasPrefix(partContentType, "image/") || isFinalErrorInlineImage(string(data))
		if !utf8.Valid(data) || strings.EqualFold(partContentType, "application/octet-stream") {
			drop = true
		}
		if drop {
			changed = true
			continue
		}
		fields[name] = append(fields[name], string(data))
	}
	if !changed && !canonicalize {
		return body, false, nil
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, false, err
	}
	return encoded, true, nil
}

func isFinalErrorJSONMediaType(mediaType string) bool {
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func isImageDiagnosticRequestURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err == nil && parsed != nil {
		raw = parsed.Path
	}
	path := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasSuffix(path, "/images/generations") || strings.HasSuffix(path, "/images/edits")
}
