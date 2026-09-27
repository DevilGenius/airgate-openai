package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type failoverProbeWriter struct {
	*httptest.ResponseRecorder
	committed bool
}

func (w *failoverProbeWriter) WriteHeader(code int) {
	w.committed = true
	w.ResponseRecorder.WriteHeader(code)
}
func (w *failoverProbeWriter) Write(p []byte) (int, error) {
	w.committed = true
	return w.ResponseRecorder.Write(p)
}
func (w *failoverProbeWriter) Flush() { w.committed = true; w.ResponseRecorder.Flush() }

func TestBasispointsUsesCoreRetryOutcomesBeforeOutput(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []string{"http_limit", "stream_limit", "early_eof", "http_server", "bad_request", "late_limit", "late_eof"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", protocol, stream, scenario), func(t *testing.T) {
					req := bpsRequest()
					req.Stream = stream
					writer := &failoverProbeWriter{ResponseRecorder: httptest.NewRecorder()}
					req.Writer = writer
					body := req.Body
					if protocol == "chat" {
						req.Headers.Set("X-Airgate-Request-Path", "/v1/chat/completions")
						req.Body = []byte("{\"model\":\"gpt-5.6-sol\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
					}
					rawError := map[string]any{"type": "usage_limit_reached", "code": "usage_limit_reached", "message": "The usage limit has been reached", "resets_in_seconds": 120}
					encode := func(v any) string {
						raw, err := json.Marshal(v)
						if err != nil {
							t.Fatal(err)
						}
						return string(raw)
					}
					limitBody := encode(map[string]any{"error": rawError})
					created := encode(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_retry", "model": req.Model, "status": "in_progress"}})
					failed := encode(map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_retry", "model": req.Model, "status": "failed", "error": rawError, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}})
					delta := encode(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "partial"})
					g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
						switch scenario {
						case "http_limit":
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(400)
							fmt.Fprint(w, limitBody)
							return
						case "http_server":
							w.WriteHeader(503)
							fmt.Fprint(w, "temporary upstream failure")
							return
						case "bad_request":
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(400)
							fmt.Fprint(w, "{\"error\":{\"type\":\"invalid_request_error\",\"message\":\"invalid parameter\"}}")
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: "+created+"\n\n")
						if strings.HasPrefix(scenario, "late_") {
							fmt.Fprint(w, "data: "+delta+"\n\n")
						}
						if strings.HasSuffix(scenario, "limit") {
							fmt.Fprint(w, "data: "+failed+"\n\n")
						}
					})
					var outcome sdk.ForwardOutcome
					if protocol == "anthropic" {
						var used bool
						outcome, used = g.tryBasispointsAnthropic(context.Background(), req, body, "claude-sonnet", req.Model, time.Now(), writer)
						if !used {
							t.Fatal("BPS was not used")
						}
					} else {
						var used bool
						var err error
						outcome, used, err = g.tryBasispointsOAuth(context.Background(), req)
						if !used || err != nil {
							t.Fatalf("used=%v err=%v", used, err)
						}
					}
					want := sdk.OutcomeUpstreamTransient
					if strings.HasSuffix(scenario, "limit") {
						want = sdk.OutcomeAccountRateLimited
					}
					if scenario == "bad_request" {
						want = sdk.OutcomeClientError
					}
					committed := stream && strings.HasPrefix(scenario, "late_")
					if committed {
						want = sdk.OutcomeStreamAborted
					}
					if outcome.Kind != want {
						t.Fatalf("kind=%s want=%s reason=%s", outcome.Kind, want, outcome.Reason)
					}
					if writer.committed != committed {
						t.Fatalf("committed=%v want=%v body=%s", writer.committed, committed, writer.Body.String())
					}
					shouldRetry := !committed && scenario != "bad_request"
					if outcome.ShouldFailover() != shouldRetry {
						t.Fatalf("retry=%v want=%v", outcome.ShouldFailover(), shouldRetry)
					}
					if want == sdk.OutcomeAccountRateLimited && outcome.RetryAfter != 120*time.Second {
						t.Fatalf("reset lost: %s", outcome.RetryAfter)
					}
					if scenario == "stream_limit" || scenario == "late_limit" {
						if outcome.Usage == nil || outcome.Usage.InputTokens != 10 || outcome.Usage.OutputTokens != 2 {
							t.Fatalf("failure usage lost: %+v", outcome.Usage)
						}
					}
				})
			}
		}
	}
}

func TestResponsesFailureOutcomeSharedBetweenNativeAndBPS(t *testing.T) {
	raw := []byte("{\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"usage_limit_reached\",\"message\":\"The usage limit has been reached\",\"resets_in_seconds\":120}}}")
	failure := parseResponsesFailureEvent("response.failed", raw)
	for _, err := range []error{failure, io.ErrUnexpectedEOF, errors.New("dial timeout")} {
		before := responseFailureOutcome(err, raw, false, time.Second)
		after := responseFailureOutcome(err, raw, true, time.Second)
		if !before.ShouldFailover() || after.ShouldFailover() || after.Kind != sdk.OutcomeStreamAborted {
			t.Fatalf("bad retry boundary: before=%+v after=%+v", before, after)
		}
	}
}
