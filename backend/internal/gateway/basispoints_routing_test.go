package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestBasispointsSelectedTransportConvertsAllProtocols(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		for _, stream := range []bool{false, true} {
			for _, tier := range []any{"default", "fast", "priority", "flex", "auto", nil, ""} {
				t.Run(fmt.Sprintf("%s/stream=%t/tier=%v", protocol, stream, tier), func(t *testing.T) {
					req := bpsRequest()
					req.Stream = stream
					req.Writer = httptest.NewRecorder()
					req.Headers.Set("X-Airgate-Service-Tier", "fast")
					payload := map[string]any{
						"model": req.Model, "input": "hello", "service_tier": tier,
						"temperature": 0.5, "top_p": 0.9, "max_output_tokens": 30,
						"max_tokens": 30, "max_completion_tokens": 30, "truncation": "auto", "background": true,
						"tools": []any{map[string]any{"type": "web_search"}},
					}
					if protocol != "responses" {
						delete(payload, "input")
						payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
					}
					if protocol == "chat" {
						req.Headers.Set("X-Airgate-Request-Path", "/v1/chat/completions")
					}
					var err error
					req.Body, err = json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					original := bytes.Clone(req.Body)
					calls := 0
					g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
						calls++
						wire, _ := io.ReadAll(r.Body)
						for _, field := range []string{"service_tier", "temperature", "top_p", "max_output_tokens", "max_tokens", "max_completion_tokens", "truncation", "background", "tools"} {
							if gjson.GetBytes(wire, field).Exists() {
								t.Errorf("BAS wire leaked %s", field)
							}
						}
						if !strings.Contains(string(wire), "hello") {
							t.Error("input lost")
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, bpsCompleted)
					})
					var outcome sdk.ForwardOutcome
					var used bool
					if protocol == "anthropic" {
						body, convertErr := basispointsResponsesBody(req)
						if convertErr != nil {
							t.Fatal(convertErr)
						}
						outcome, used = g.tryBasispointsAnthropic(context.Background(), req, body, "claude-sonnet", req.Model, time.Now(), req.Writer)
					} else {
						outcome, used, err = g.tryBasispointsOAuth(context.Background(), req)
					}
					if err != nil || !used || outcome.Kind != sdk.OutcomeSuccess || calls != 1 || !bytes.Equal(original, req.Body) {
						t.Fatalf("BAS not preserved: kind=%s used=%v calls=%d err=%v", outcome.Kind, used, calls, err)
					}
				})
			}
		}
	}
}

func TestBasispointsInvalidRequestDoesNotFallBack(t *testing.T) {
	for _, body := range []string{`{"model":"gpt-5.6-sol","input":"hello","previous_response_id":"resp_native"}`, `{"model":"gpt-5.6-sol","input":"hello","tool_choice":"required"}`} {
		for _, anthropic := range []bool{false, true} {
			req := bpsRequest()
			req.Body = []byte(body)
			g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
				t.Error("invalid request reached upstream")
				w.WriteHeader(500)
			})
			var outcome sdk.ForwardOutcome
			var used bool
			if anthropic {
				outcome, used = g.tryBasispointsAnthropic(context.Background(), req, req.Body, "claude-sonnet", req.Model, time.Now(), nil)
			} else {
				var err error
				outcome, used, err = g.tryBasispointsOAuth(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !used || outcome.Kind != sdk.OutcomeClientError || outcome.Upstream.StatusCode != 400 {
				t.Fatalf("expected explicit BAS validation error: used=%v outcome=%+v", used, outcome)
			}
		}
	}
}
