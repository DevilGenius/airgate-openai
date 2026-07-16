package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSSEEventWriterForwardsFailureAfterCreated(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writer := &sseEventWriter{
		w:       recorder,
		flusher: recorder,
		timing:  newResponseEventTiming(time.Now()),
	}

	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_abc"}}`))
	if got := recorder.Body.String(); !writer.wrote || !strings.Contains(got, "event: response.created") {
		t.Fatalf("response.created should be forwarded immediately, stream = %q", got)
	}
	writer.OnRawEvent("response.failed", []byte(`{"type":"response.failed","response":{"id":"resp_abc","status":"failed","error":{"type":"invalid_request_error","code":"context_too_large","message":"too large"}}}`))

	got := recorder.Body.String()
	if !strings.Contains(got, "event: response.created") {
		t.Fatalf("stream = %q, want response.created", got)
	}
	if !strings.Contains(got, "event: response.failed") {
		t.Fatalf("stream = %q, want response.failed", got)
	}
	if !strings.Contains(got, `"code":"context_too_large"`) {
		t.Fatalf("stream = %q, want context_too_large", got)
	}
	if strings.Contains(got, "[DONE]") {
		t.Fatalf("stream = %q, should not include done marker after failure", got)
	}
	if !writer.terminalErrorForwarded {
		t.Fatal("terminalErrorForwarded = false, want true")
	}
}

func TestSSEEventWriterSynthesizesFailureAfterCreated(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writer := &sseEventWriter{
		w:       recorder,
		flusher: recorder,
		timing:  newResponseEventTiming(time.Now()),
	}

	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_abc"}}`))
	writer.writeTerminalErrorIfNeeded(http.StatusBadRequest, "context_too_large", contextTooLargeMessage, "resp_abc")
	writer.writeTerminalErrorIfNeeded(http.StatusBadRequest, "context_too_large", contextTooLargeMessage, "resp_abc")

	got := recorder.Body.String()
	if count := strings.Count(got, "event: response.failed"); count != 1 {
		t.Fatalf("response.failed count = %d, want 1; stream = %q", count, got)
	}
	for _, want := range []string{
		`"type":"response.failed"`,
		`"id":"resp_abc"`,
		`"status":"failed"`,
		`"type":"invalid_request_error"`,
		`"code":"context_too_large"`,
		contextTooLargeMessage,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream = %q, want contain %q", got, want)
		}
	}
}

func TestSSEEventWriterNormalizesInvalidImageFailure(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writer := &sseEventWriter{
		w:       recorder,
		flusher: recorder,
		timing:  newResponseEventTiming(time.Now()),
	}
	upstreamMessage := "The image data you provided does not represent a valid image. Please check your input and try again with one of the supported image formats: ['image/jpeg', 'image/png', 'image/gif', 'image/webp']."

	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_image"}}`))
	writer.OnRawEvent("response.failed", []byte(`{"type":"response.failed","response":{"id":"resp_image","status":"failed","error":{"type":"invalid_request_error","message":"`+upstreamMessage+`"}}}`))

	got := recorder.Body.String()
	for _, want := range []string{invalidImageInputCode, invalidImageInputMessage} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream = %q, want contain %q", got, want)
		}
	}
}
