package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestBasispointsIncompletePreservesUsageAndNeverReplays(t *testing.T) {
	const terminal = "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_limit\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[],\"usage\":{\"input_tokens\":12,\"output_tokens\":128000,\"input_tokens_details\":{\"cached_tokens\":4},\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n"
	for _, chat := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, prior := range []string{"none", "text", "reasoning"} {
				t.Run(fmt.Sprintf("chat=%v/stream=%v/prior=%s", chat, stream, prior), func(t *testing.T) {
					req := bpsRequest()
					req.Stream = stream
					writer := httptest.NewRecorder()
					req.Writer = writer
					if chat {
						req.Headers.Set("X-Airgate-Request-Path", "/v1/chat/completions")
						req.Body = []byte("{\"model\":\"gpt-5.6-sol\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
					}
					var calls atomic.Int32
					g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_limit\",\"status\":\"in_progress\"}}\n\n")
						if prior != "none" {
							kind := "response.output_text.delta"
							if prior == "reasoning" {
								kind = "response.reasoning_summary_text.delta"
							}
							_, _ = fmt.Fprintf(w, "data: {\"type\":%q,\"output_index\":0,\"delta\":\"partial\"}\n\n", kind)
						}
						_, _ = io.WriteString(w, terminal)
					})
					outcome, used, err := g.tryBasispointsOAuth(context.Background(), req)
					want := sdk.OutcomeSuccess
					if stream && !chat {
						want = sdk.OutcomeStreamAborted
					}
					if !used || err != nil || outcome.Kind != want || outcome.ShouldFailover() || calls.Load() != 1 {
						t.Fatalf("incorrect terminal outcome: %+v, used=%v err=%v calls=%d", outcome, used, err, calls.Load())
					}
					if outcome.Usage == nil || outcome.Usage.OutputTokens != 128000 || outcome.Usage.InputTokens != 8 || outcome.Usage.CachedInputTokens != 4 {
						t.Fatalf("incomplete usage lost: %+v", outcome.Usage)
					}
					payload := string(outcome.Upstream.Body)
					if stream {
						payload = writer.Body.String()
					}
					if chat {
						if !strings.Contains(payload, "\"finish_reason\":\"length\"") {
							t.Fatalf("Chat length semantics changed: %s", payload)
						}
					} else if stream {
						if outcome.Reason != "response.incomplete: max_output_tokens" || strings.Count(payload, "event: response.incomplete\n") != 1 || strings.Contains(payload, "response.completed") || strings.Contains(payload, "response.failed") || strings.Contains(payload, "[DONE]") {
							t.Fatalf("incomplete was hidden, duplicated or replaced: %s", payload)
						}
					} else if gjson.Get(payload, "status").String() != "incomplete" || gjson.Get(payload, "incomplete_details.reason").String() != "max_output_tokens" {
						t.Fatalf("Responses JSON lost original incomplete status: %s", payload)
					}
				})
			}
		}
	}
}
