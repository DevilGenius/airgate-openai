package basispoints

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"strings"
	"testing"
)

func inlineImageFixture(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
}

func TestInlineAttachmentPlanPreservesHistoryAndDeduplicates(t *testing.T) {
	dataURL := inlineImageFixture(t)
	for _, toolResult := range []bool{false, true} {
		parts := []any{object{"type": "input_image", "image_url": dataURL, "detail": "high"}, object{"type": "input_image", "image_url": dataURL}, object{"type": "input_image", "file_id": "file-existing"}, object{"type": "input_image", "image_url": "https://example.test/p.png"}}
		input := []any{object{"role": "user", "content": parts}}
		if toolResult {
			input = []any{object{"type": "function_call", "name": "view_image", "call_id": "call_image", "arguments": `{}`}, object{"type": "function_call_output", "call_id": "call_image", "output": parts}}
		}
		source := testSource()
		source["input"] = input
		raw, _ := json.Marshal(source)
		before := bytes.Clone(raw)
		prepared, err := PrepareRequest(raw, "account:key")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(prepared.Body()); err == nil {
			t.Fatal("unuploaded placeholders can be sent")
		}
		count := 0
		err = prepared.UploadImages(context.Background(), func(_ context.Context, img InlineImage) (string, error) {
			count++
			if img.MediaType != "image/png" {
				t.Errorf("media type = %s", img.MediaType)
			}
			if _, err := png.Decode(bytes.NewReader(img.Data)); err != nil {
				t.Error(err)
			}
			return "file-uploaded", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		wire, err := io.ReadAll(prepared.Body())
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 || !bytes.Equal(raw, before) || bytes.Contains(wire, []byte("data:image")) || bytes.Contains(wire, []byte(inlineAttachmentPrefix)) {
			t.Fatalf("bad image plan: uploads=%d mutation=%v", count, !bytes.Equal(raw, before))
		}
		for _, value := range []string{`"file_id":"file-uploaded"`, `"file_id":"file-existing"`, "https://example.test/p.png", `"detail":"high"`} {
			if !bytes.Contains(wire, []byte(value)) {
				t.Errorf("missing %s", value)
			}
		}
		if err := prepared.UploadImages(context.Background(), func(context.Context, InlineImage) (string, error) { t.Error("uploaded twice"); return "", nil }); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInlineAttachmentValidatesWholeRequestBeforeUpload(t *testing.T) {
	source := testSource()
	source["input"] = []any{object{"role": "user", "content": []any{object{"type": "input_image", "image_url": inlineImageFixture(t)}, object{"type": "encrypted_content", "encrypted_content": "private"}}}}
	raw, _ := json.Marshal(source)
	if p, err := PrepareRequest(raw, "account"); p != nil || err == nil {
		t.Fatal("invalid history accepted")
	}
	for _, data := range []string{"data:image/png;base64,YQ==", "data:image/png;base64,!!", "data:image/svg+xml,<svg/>", "data:image/png;base64"} {
		if _, err := decodeInlineImage(data); err == nil {
			t.Errorf("invalid image accepted: %q", data)
		}
	}
}

func TestInlineAttachmentFailureAndCancellationNeverExposePlaceholders(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		source := testSource()
		source["input"] = []any{object{"role": "user", "content": []any{object{"type": "input_image", "image_url": inlineImageFixture(t)}}}}
		raw, _ := json.Marshal(source)
		p, err := PrepareRequest(raw, "account")
		if err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithCancel(context.Background())
		if cancel {
			stop()
		}
		err = p.UploadImages(ctx, func(context.Context, InlineImage) (string, error) {
			if cancel {
				t.Error("upload after cancellation")
			}
			return "", errors.New("failed")
		})
		stop()
		if err == nil {
			t.Fatal("failure lost")
		}
		if body, readErr := io.ReadAll(p.Body()); len(body) != 0 || readErr == nil {
			t.Fatal("failed upload leaked placeholders")
		}
	}
}

func TestInlineAttachmentDoesNotRewriteToolArgumentData(t *testing.T) {
	source := testSource()
	source["input"] = []any{object{"type": "function_call", "call_id": "call_app", "name": "app", "arguments": `{"type":"input_image","image_url":"data:image/png;base64,not-an-image"}`}, object{"type": "function_call_output", "call_id": "call_app", "output": "done"}}
	raw, _ := json.Marshal(source)
	body, images, err := planInlineImages(raw)
	if err != nil || len(images) != 0 || !bytes.Equal(body, raw) || !strings.Contains(string(body), "not-an-image") {
		t.Fatal("application data changed")
	}
}
