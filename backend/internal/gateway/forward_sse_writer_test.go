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
		start:   time.Now(),
	}

	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_abc"}}`))
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

func TestSSEEventWriterDelaysCreatedUntilOutput(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writer := &sseEventWriter{
		w:            recorder,
		flusher:      recorder,
		start:        time.Now(),
		delayCreated: true,
	}

	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_abc"}}`))
	if got := recorder.Body.String(); got != "" {
		t.Fatalf("stream = %q, want empty before output", got)
	}
	if writer.wrote {
		t.Fatal("wrote = true, want false while response.created is pending")
	}

	writer.OnRawEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"hi"}`))

	got := recorder.Body.String()
	createdIdx := strings.Index(got, "event: response.created")
	deltaIdx := strings.Index(got, "event: response.output_text.delta")
	if createdIdx < 0 || deltaIdx < 0 || createdIdx > deltaIdx {
		t.Fatalf("stream = %q, want delayed created flushed before delta", got)
	}
	if !writer.wrote {
		t.Fatal("wrote = false, want true after output")
	}
}

func TestSSEEventWriterDelayedCreatedSuppressesContextTooLarge(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writer := &sseEventWriter{
		w:            recorder,
		flusher:      recorder,
		start:        time.Now(),
		delayCreated: true,
	}

	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_abc"}}`))
	writer.OnRawEvent("response.failed", []byte(`{"type":"response.failed","response":{"id":"resp_abc","status":"failed","error":{"type":"invalid_request_error","code":"context_too_large","message":"too large"}}}`))

	if got := recorder.Body.String(); got != "" {
		t.Fatalf("stream = %q, want empty so Core can dispatch-candidate failover", got)
	}
	if writer.wrote {
		t.Fatal("wrote = true, want false when delayed created is followed by context_too_large")
	}
	if writer.terminalErrorForwarded {
		t.Fatal("terminalErrorForwarded = true, want false")
	}
}

func TestSSEEventWriterDelayedCreatedFlushesNonContextFailure(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writer := &sseEventWriter{
		w:            recorder,
		flusher:      recorder,
		start:        time.Now(),
		delayCreated: true,
	}

	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_abc"}}`))
	writer.OnRawEvent("response.failed", []byte(`{"type":"response.failed","response":{"id":"resp_abc","status":"failed","error":{"type":"server_error","code":"server_overloaded","message":"overloaded"}}}`))

	got := recorder.Body.String()
	if !strings.Contains(got, "event: response.created") || !strings.Contains(got, "event: response.failed") {
		t.Fatalf("stream = %q, want created and non-context failure", got)
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
		start:   time.Now(),
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
