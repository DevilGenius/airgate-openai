package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/DevilGenius/airgate-openai/backend/internal/basispoints"
)

// Preserve upload HTTP status for normal account/rate-limit classification.
type basispointsAttachmentHTTPError struct{ response *http.Response }

func (e *basispointsAttachmentHTTPError) Error() string {
	return fmt.Sprintf("BAS attachment upload returned HTTP %d", e.response.StatusCode)
}

func uploadBasispointsImage(ctx context.Context, client *http.Client, headers http.Header, image basispoints.InlineImage) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	partHeaders := make(textproto.MIMEHeader)
	partHeaders.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": "image." + strings.TrimPrefix(image.MediaType, "image/")}))
	partHeaders.Set("Content-Type", image.MediaType)
	part, err := writer.CreatePart(partHeaders)
	if err != nil {
		return "", err
	}
	if _, err = part.Write(image.Data); err != nil {
		return "", err
	}
	if err = writer.Close(); err != nil {
		return "", err
	}
	uploadCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	endpoint := strings.TrimSuffix(basispoints.ResponsesURL, "/responses") + "/attachments"
	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPost, endpoint, bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", err
	}
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := readLimitedErrorBody(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return "", &basispointsAttachmentHTTPError{response: resp}
	}
	fileID := strings.TrimSpace(gjson.GetBytes(raw, "openai_file_id").String())
	if !basispoints.ValidAttachmentID(fileID) {
		return "", fmt.Errorf("BAS attachment upload returned no valid openai_file_id")
	}
	return fileID, nil
}
