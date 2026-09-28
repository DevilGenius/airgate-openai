package basispoints

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

const maxInlineImageBytes = 20 << 20
const maxInlineImagesBytes = 50 << 20
const inlineAttachmentPrefix = "file-airgate-inline-"

// InlineImage is request-local. Neither image bytes nor credentials are cached.
type InlineImage struct {
	MediaType string
	Data      []byte
}

type pendingImageReader struct{}

func (pendingImageReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("inline images must be uploaded before sending the BAS request")
}

func decodeInlineImage(raw string) (InlineImage, error) {
	metadata, encoded, ok := strings.Cut(raw[5:], ",")
	if !ok || len(encoded) > maxInlineImageBytes*3 {
		return InlineImage{}, fmt.Errorf("invalid or oversized inline image")
	}
	isBase64 := strings.HasSuffix(strings.ToLower(metadata), ";base64")
	if isBase64 {
		metadata = metadata[:len(metadata)-len(";base64")]
	}
	mediaType, _, err := mime.ParseMediaType(metadata)
	if err != nil {
		return InlineImage{}, fmt.Errorf("invalid inline image media type")
	}
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return InlineImage{}, fmt.Errorf("inline images must be PNG, JPEG, GIF or WebP")
	}
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		return InlineImage{}, fmt.Errorf("invalid inline image encoding")
	}
	data := []byte(decoded)
	if isBase64 {
		data, err = base64.StdEncoding.DecodeString(decoded)
	}
	if err != nil || len(data) == 0 || len(data) > maxInlineImageBytes {
		return InlineImage{}, fmt.Errorf("inline image is invalid or exceeds 20 MiB")
	}
	if http.DetectContentType(data) != mediaType {
		return InlineImage{}, fmt.Errorf("inline image media type does not match its contents")
	}
	return InlineImage{MediaType: mediaType, Data: data}, nil
}

// Plan images only in protocol content/output arrays, never tool arguments or
// schemas. Stable placeholders allow all history validation before any upload.
func planInlineImages(raw []byte) ([]byte, map[string]InlineImage, error) {
	var source object
	if err := decode(raw, &source); err != nil || source == nil {
		return nil, nil, fmt.Errorf("invalid Basispoints request JSON")
	}
	images := make(map[string]InlineImage)
	total := 0
	input, _ := source["input"].([]any)
	for _, value := range input {
		item, _ := value.(object)
		field := "content"
		switch text(item["type"]) {
		case "", "message", "agent_message":
		case "function_call_output", "custom_tool_call_output":
			field = "output"
		default:
			continue
		}
		parts, _ := item[field].([]any)
		for _, value := range parts {
			part, _ := value.(object)
			if text(part["type"]) != "input_image" {
				continue
			}
			if strings.HasPrefix(text(part["file_id"]), inlineAttachmentPrefix) {
				return nil, nil, fmt.Errorf("reserved inline attachment reference")
			}
			imageURL := text(part["image_url"])
			if !strings.HasPrefix(strings.ToLower(imageURL), "data:") {
				continue
			}
			if _, exists := part["file_id"]; exists {
				return nil, nil, fmt.Errorf("input_image requires exactly one image reference")
			}
			image, err := decodeInlineImage(imageURL)
			if err != nil {
				return nil, nil, err
			}
			key := fmt.Sprintf("%s%x", inlineAttachmentPrefix, sha256.Sum256(image.Data))
			if _, exists := images[key]; !exists {
				total += len(image.Data)
			}
			if total > maxInlineImagesBytes {
				return nil, nil, fmt.Errorf("inline images exceed 50 MiB per request")
			}
			images[key] = image
			delete(part, "image_url")
			part["file_id"] = key
		}
	}
	if len(images) == 0 {
		return raw, nil, nil
	}
	body, err := json.Marshal(source)
	return body, images, err
}

// UploadImages resolves only this request's placeholders, using the selected
// account's uploader. Duplicate images upload once; failed plans are never sent.
func (r *PreparedRequest) UploadImages(ctx context.Context, upload func(context.Context, InlineImage) (string, error)) error {
	if len(r.images) == 0 {
		return nil
	}
	var body object
	if err := decode(r.body, &body); err != nil {
		return err
	}
	uploaded := make(map[string]string)
	input, _ := body["input"].([]any)
	for _, value := range input {
		item, _ := value.(object)
		parts, _ := item["content"].([]any)
		for _, value := range parts {
			part, _ := value.(object)
			if text(part["type"]) != "input_image" {
				continue
			}
			key := text(part["file_id"])
			image, pending := r.images[key]
			if !pending {
				continue
			}
			fileID := uploaded[key]
			if fileID == "" {
				if err := ctx.Err(); err != nil {
					return err
				}
				var err error
				fileID, err = upload(ctx, image)
				if err != nil {
					return err
				}
				if !ValidAttachmentID(fileID) {
					return fmt.Errorf("BAS upload returned an invalid attachment ID")
				}
				uploaded[key] = fileID
			}
			part["file_id"] = fileID
			if _, exists := part["detail"]; !exists {
				part["detail"] = "auto"
			}
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	r.body, r.images = encoded, nil
	return nil
}
