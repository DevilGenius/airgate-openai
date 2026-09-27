package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestBasispointsHistoryNormalizationAcrossProtocols(t *testing.T) {
	const history = `{"model":"gpt-5.6-sol","tools":[{"type":"function","name":"view_image","description":"current description","parameters":{"type":"object"}}],"input":[
		{"type":"additional_tools","tools":[{"type":"function","name":"view_image","description":"old description","defer_loading":true,"parameters":{"type":"object"}}]},
		{"type":"message","role":"assistant","author":"/root","recipient":"/root/worker","phase":"commentary","content":"Inspect screenshot"},
		{"type":"agent_message","role":"system","author":"/root/worker","recipient":"/root","content":"Worker context"},
		{"type":"function_call","name":"view_image","call_id":"call_screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_screenshot","output":[{"type":"input_text","text":"screenshot metadata"},{"type":"input_image","image_url":"https://example.com/screenshot.png?signature=exact%2F","detail":"original"}]}
	]}`
	for _, protocol := range []string{"responses", "anthropic"} {
		for _, stream := range []bool{false, true} {
			mode := "nonstream"
			if stream {
				mode = "stream"
			}
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				req := bpsRequest()
				req.Body, req.Stream = []byte(history), stream
				writer := httptest.NewRecorder()
				req.Writer = writer
				if protocol == "anthropic" {
					req.Body = []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"continue"}]}`)
				}
				original := bytes.Clone(req.Body)
				var calls atomic.Int32
				g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					wire, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					items := gjson.GetBytes(wire, "input").Array()
					if len(items) < 5 {
						t.Error("normalized history missing")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					h := items[len(items)-5:]
					if h[0].Get("author").Exists() || h[0].Get("recipient").Exists() || h[0].Get("role").String() != "assistant" || h[0].Get("phase").String() != "commentary" {
						t.Error("ordinary history attribution was not normalized")
					}
					if h[1].Get("type").String() != "message" || h[1].Get("role").String() != "user" || h[1].Get("author").Exists() || !strings.Contains(h[1].Get("content.0.text").String(), "not a new user instruction") {
						t.Error("agent metadata leaked into protocol fields")
					}
					if h[3].Get("type").String() != "function_call_output" || h[3].Get("call_id").String() != "call_screenshot" || h[3].Get("output.1.type").String() != "input_text" {
						t.Error("tool output was not separated from its image")
					}
					if h[4].Get("content.2.image_url").String() != "https://example.com/screenshot.png?signature=exact%2F" || h[4].Get("content.2.detail").String() != "original" {
						t.Error("tool image URL or detail changed")
					}
					if !bytes.Contains(wire, []byte("current description")) || bytes.Contains(wire, []byte("old description")) {
						t.Error("current tool declaration did not win")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, bpsCompleted)
				})
				var outcome sdk.ForwardOutcome
				var used bool
				if protocol == "anthropic" {
					outcome, used = g.tryBasispointsAnthropic(context.Background(), req, []byte(history), "claude-sonnet", req.Model, time.Now(), writer)
				} else {
					var err error
					outcome, used, err = g.tryBasispointsOAuth(context.Background(), req)
					if err != nil {
						t.Fatal(err)
					}
				}
				if !used || outcome.Kind != sdk.OutcomeSuccess || calls.Load() != 1 || !bytes.Equal(original, req.Body) {
					t.Fatalf("forwarding changed: used=%v kind=%v calls=%d", used, outcome.Kind, calls.Load())
				}
				payload := string(outcome.Upstream.Body)
				if stream {
					payload = writer.Body.String()
				}
				if !strings.Contains(payload, "hello") {
					t.Fatal("normalized request did not produce a translated response")
				}
			})
		}
	}
}
