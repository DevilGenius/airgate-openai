package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/DevilGenius/airgate-openai/backend/internal/basispoints"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func consumeKeepaliveFixture(t *testing.T, protocol string, ctx context.Context, body io.ReadCloser, w http.ResponseWriter) error {
	t.Helper()
	if protocol == "anthropic" {
		outcome, err := translateResponsesSSEToAnthropicSSE(ctx, &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, w, "claude-sonnet", "gpt-test", nil, "", "", time.Now(), 0, openAISessionResolution{})
		if err == nil && outcome.Kind != sdk.OutcomeSuccess {
			err = errors.New(outcome.Reason)
		}
		return err
	}
	var handler WSEventHandler
	if protocol == "chat" {
		handler = newChatCompletionsStreamWriter(w, "gpt-test", 0, "", false, time.Now())
	} else {
		s := &sseEventWriter{w: w, timing: newResponseEventTiming(time.Now())}
		s.flusher, _ = w.(http.Flusher)
		handler = s
	}
	return ParseSSEStream(body, handler, ctx).Err
}

func TestBPSKeepaliveDuringBaselineValidationWait(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			prepared, reason := basispoints.PrepareRequest([]byte("{\"model\":\"gpt-test\",\"input\":\"hi\"}"), "keepalive")
			if reason != nil {
				t.Fatal(reason)
			}
			raw, producer := io.Pipe()
			defer func() { _ = producer.Close() }()
			body := newBasispointsKeepaliveBody(ctx, prepared.Stream(ctx, raw), 5*time.Millisecond)
			defer func() { _ = body.Close() }()
			go func() {
				_, _ = io.WriteString(producer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_bps\"}}\n\n")
				_, _ = io.WriteString(producer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
				for range 15 {
					// Raw traffic can continue while the unchanged bridge withholds tools.
					if _, err := io.WriteString(producer, ": upstream alive\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"PRIVATE_NATIVE_ENVELOPE\"}\n\n"); err != nil {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
				_, _ = io.WriteString(producer, bpsCompleted)
			}()
			w := httptest.NewRecorder()
			if err := consumeKeepaliveFixture(t, protocol, ctx, body, w); err != nil {
				t.Fatal(err)
			}
			out := w.Body.String()
			if !strings.Contains(out, "hello") || strings.Contains(out, "PRIVATE_NATIVE_ENVELOPE") || strings.Contains(out, basispointsKeepaliveEventType) {
				t.Fatal("baseline content or private heartbeat boundary changed")
			}
			found := false
			for _, line := range strings.Split(out, "\n") {
				data, ok := extractSSEData(line)
				if !ok || data == "[DONE]" {
					continue
				}
				if !gjson.Valid(data) {
					t.Fatal("heartbeat split a JSON event")
				}
				kind := gjson.Get(data, "type").String()
				if kind == "keepalive" || kind == "ping" || protocol == "chat" && gjson.Get(data, "choices.0.delta").Raw == "{}" {
					found = true
				}
			}
			if !found {
				t.Fatal("filtered stream had no downstream keepalive")
			}
		})
	}
}

func TestBPSKeepalivePreservesUncommittedRetry(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			raw, producer := io.Pipe()
			defer func() { _ = producer.Close() }()
			body := newBasispointsKeepaliveBody(ctx, raw, 3*time.Millisecond)
			defer func() { _ = body.Close() }()
			go func() {
				_, _ = io.WriteString(producer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_wait\"}}\n\n")
				time.Sleep(40 * time.Millisecond)
				_, _ = io.WriteString(producer, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"unavailable\"}}}\n\n")
			}()
			w := httptest.NewRecorder()
			err := consumeKeepaliveFixture(t, protocol, ctx, body, w)
			if err == nil || w.Body.Len() != 0 || w.Flushed || !responseFailureOutcome(err, nil, false, time.Second).ShouldFailover() {
				t.Fatalf("heartbeat committed a retryable attempt: %v", err)
			}
		})
	}
}

func TestBPSKeepalivePreservesFramesAndStopsAtTerminal(t *testing.T) {
	const wire = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	body := newBasispointsKeepaliveBody(t.Context(), io.NopCloser(strings.NewReader(wire)), time.Hour)
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	if err != nil || string(got) != wire {
		t.Fatalf("baseline frames changed: %v", err)
	}
}

func TestBPSKeepaliveNeverSplitsPartialFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	raw, producer := io.Pipe()
	defer func() { _ = producer.Close() }()
	body := newBasispointsKeepaliveBody(ctx, raw, 3*time.Millisecond)
	defer func() { _ = body.Close() }()
	const terminal = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	go func() {
		_, _ = io.WriteString(producer, terminal[:50])
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(producer, terminal[50:])
	}()
	out, err := io.ReadAll(body)
	if err != nil || !strings.HasSuffix(string(out), terminal) || !strings.Contains(string(out), basispointsKeepaliveFrame) {
		t.Fatalf("terminal frame split or followed by heartbeat: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if data, ok := extractSSEData(line); ok && !gjson.Valid(data) {
			t.Fatal("heartbeat inserted inside JSON")
		}
	}
}

func TestBPSKeepalivePreservesNativeEventsAndTiming(t *testing.T) {
	w := httptest.NewRecorder()
	s := &sseEventWriter{w: w, flusher: w, timing: newResponseEventTiming(time.Now())}
	s.OnRawEvent(basispointsKeepaliveEventType, nil)
	if s.timing.firstEventRecorded || s.timing.firstTokenRecorded || s.wrote || w.Flushed {
		t.Fatal("private heartbeat changed timing or commitment")
	}
	if handled, err := handleBasispointsKeepalive("keepalive", w, true, s); handled || err != nil {
		t.Fatal("native keepalive behavior was intercepted")
	}
	silent := &responsesSilentHandler{timing: newResponseEventTiming(time.Now())}
	silent.OnRawEvent(basispointsKeepaliveEventType, nil)
	if silent.timing.firstEventRecorded {
		t.Fatal("private heartbeat entered collected response timing")
	}
}

func TestBPSKeepaliveCancellationClosesReader(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	raw := &closeOnlyReader{closed: make(chan struct{})}
	body := newBasispointsKeepaliveBody(ctx, raw, time.Hour).(*basispointsKeepaliveBody)
	defer func() { _ = body.Close() }()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(body); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reader did not cancel")
	}
	select {
	case <-raw.closed:
	case <-time.After(time.Second):
		t.Fatal("underlying body remained occupied")
	}
	select {
	case <-body.frames:
	case <-time.After(time.Second):
		t.Fatal("frame worker did not exit")
	}
}

type baselineKeepaliveConfig struct{ sdk.PluginConfig }

func (baselineKeepaliveConfig) GetDuration(key string) time.Duration {
	if key == "first_byte_timeout" {
		return 50 * time.Millisecond
	}
	if key == "stream_idle_timeout" {
		return 80 * time.Millisecond
	}
	return 0
}

type baselineKeepaliveContext struct{}

func (baselineKeepaliveContext) Config() sdk.PluginConfig { return baselineKeepaliveConfig{} }
func (baselineKeepaliveContext) Logger() *slog.Logger     { return slog.Default() }

func TestBPSBaselineHiddenReasoningDoesNotTriggerRetry(t *testing.T) {
	req := bpsRequest()
	req.Stream = true
	w := httptest.NewRecorder()
	req.Writer = w
	var attempts atomic.Int32
	g := bpsGateway(t, req, func(up http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		up.Header().Set("Content-Type", "text/event-stream")
		for range 20 {
			_, err := io.WriteString(up, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_hidden\",\"summary\":[],\"encrypted_content\":\"opaque\"}}\n\n")
			if err != nil {
				return
			}
			up.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		_, _ = io.WriteString(up, bpsCompleted)
	})
	g.ctx = baselineKeepaliveContext{}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	outcome, used, err := g.tryBasispointsOAuth(ctx, req)
	if err != nil || !used || outcome.Kind != sdk.OutcomeSuccess || attempts.Load() != 1 || ctx.Err() != nil || !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("active hidden reasoning was interrupted: %+v %v", outcome, err)
	}
}

func TestBPSKeepaliveCannotMaskRawTransportIdle(t *testing.T) {
	parent, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	raw := newStallGuardBody(&idleReader{ctx: ctx}, 50*time.Millisecond, cancel)
	body := newBasispointsKeepaliveBody(parent, raw, 3*time.Millisecond)
	defer func() { _ = body.Close() }()
	_, err := io.ReadAll(body)
	if err == nil || parent.Err() != nil {
		t.Fatalf("heartbeat hid raw transport failure: %v", err)
	}
}
