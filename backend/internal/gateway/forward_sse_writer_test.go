package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSSEEventWriterSuppressesFailureBeforeOutput(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writer := &sseEventWriter{
		w:       recorder,
		flusher: recorder,
		timing:  newResponseEventTiming(time.Now()),
	}

	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_abc"}}`))
	if got := recorder.Body.String(); writer.wrote || got != "" {
		t.Fatalf("response.created should remain buffered, stream = %q", got)
	}
	writer.OnRawEvent("response.failed", []byte(`{"type":"response.failed","response":{"id":"resp_abc","status":"failed","error":{"type":"invalid_request_error","code":"context_too_large","message":"too large"}}}`))

	if got := recorder.Body.String(); writer.wrote || got != "" {
		t.Fatalf("pre-output failure should not commit the stream, stream = %q", got)
	}
	if writer.terminalErrorForwarded {
		t.Fatal("terminalErrorForwarded = true, want false")
	}
}

func TestSSEEventWriterForwardsFailureAfterOutput(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writer := &sseEventWriter{
		w:       recorder,
		flusher: recorder,
		timing:  newResponseEventTiming(time.Now()),
	}

	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_abc"}}`))
	writer.OnRawEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	writer.OnRawEvent("response.failed", []byte(`{"type":"response.failed","response":{"id":"resp_abc","status":"failed","error":{"type":"server_error","code":"server_overloaded","message":"overloaded"}}}`))

	got := recorder.Body.String()
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", "event: response.failed", `"code":"server_overloaded"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream = %q, want contain %q", got, want)
		}
	}
	if !writer.wrote || !writer.terminalErrorForwarded {
		t.Fatalf("writer state = wrote:%v terminal:%v, want true/true", writer.wrote, writer.terminalErrorForwarded)
	}
}

func TestSSEEventWriterStoresResponseIDOnlyAfterCompletion(t *testing.T) {
	t.Parallel()

	const sessionKey = "test:sse-writer-completion-anchor"
	clearSessionStateResponseID(sessionKey)
	t.Cleanup(func() { clearSessionStateResponseID(sessionKey) })

	recorder := httptest.NewRecorder()
	writer := &sseEventWriter{
		w:          recorder,
		flusher:    recorder,
		accountID:  42,
		sessionKey: sessionKey,
		timing:     newResponseEventTiming(time.Now()),
	}

	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_pending"}}`))
	if state := getSessionState(sessionKey); state != nil && state.LastResponseID != "" {
		t.Fatalf("response.created stored continuation anchor %q", state.LastResponseID)
	}
	writer.OnRawEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	writer.OnRawEvent("response.completed", []byte(`{"type":"response.completed","response":{"id":"resp_done","status":"completed"}}`))

	state := getSessionState(sessionKey)
	if state == nil || state.LastResponseID != "resp_done" || state.AccountID != 42 {
		t.Fatalf("completed session state = %+v, want resp_done on account 42", state)
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
	writer.OnRawEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
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
	writer.OnRawEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	writer.OnRawEvent("response.failed", []byte(`{"type":"response.failed","response":{"id":"resp_image","status":"failed","error":{"type":"invalid_request_error","message":"`+upstreamMessage+`"}}}`))

	got := recorder.Body.String()
	for _, want := range []string{invalidImageInputCode, invalidImageInputMessage} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream = %q, want contain %q", got, want)
		}
	}
}
