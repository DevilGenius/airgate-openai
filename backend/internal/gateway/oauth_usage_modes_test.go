package gateway

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/DevilGenius/airgate-openai/backend/internal/model"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestOAuthAndBasispointsDoNotInferCacheCreation(t *testing.T) {
	for _, cached := range []int{0, 200, 1000} {
		t.Run(fmt.Sprintf("cached=%d", cached), func(t *testing.T) {
			rawUsage := cacheUsageFixture(t, map[string]int{"input_tokens_details.cached_tokens": cached})
			event := strings.TrimSpace(strings.TrimPrefix(bpsCompleted, "data: "))
			event, err := sjson.SetRaw(event, "response.usage", string(rawUsage))
			if err != nil {
				t.Fatal(err)
			}
			wire := "data: " + event + "\n\n"
			for _, transport := range []string{"native_websocket", "native_sse", "basispoints_sse"} {
				t.Run(transport, func(t *testing.T) {
					var usage *sdk.Usage
					if transport == "basispoints_sse" {
						req := bpsRequest()
						g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, wire)
						})
						outcome, used, err := g.tryBasispointsOAuth(context.Background(), req)
						if err != nil || !used || outcome.Kind != sdk.OutcomeSuccess {
							t.Fatalf("BPS outcome=%+v used=%v err=%v", outcome, used, err)
						}
						usage = outcome.Usage
					} else {
						var result WSResult
						if transport == "native_websocket" {
							result = receiveNativeUsageFixture(t, event)
						} else {
							result = ParseSSEStream(strings.NewReader(wire), nil, context.Background())
						}
						if result.Err != nil {
							t.Fatal(result.Err)
						}
						usage = newTokenUsage("gpt-5.6-sol", "", result.InputTokens, result.OutputTokens, result.CachedInputTokens, result.CacheCreationTokens, result.ReasoningOutputTokens, 0)
						fillUsageCost(usage)
						// Chat rebuilding must not convert the uncached portion into cache writes.
						chat := gjson.GetBytes(buildNonStreamChatCompletion(result, "gpt-5.6-sol"), "usage")
						if chat.Get("prompt_tokens").Int() != 1000 || chat.Get("prompt_tokens_details.cache_write_tokens").Int() != 0 {
							t.Fatalf("invented Chat cache writes: %s", chat.Raw)
						}
					}
					assertNoInferredCreation(t, usage, cached)
				})
			}
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("native_anthropic/stream=%t", stream), func(t *testing.T) {
					resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
					defer func() { _ = resp.Body.Close() }()
					var outcome sdk.ForwardOutcome
					var err error
					w := httptest.NewRecorder()
					original := []byte("{}")
					if stream {
						outcome, err = translateResponsesSSEToAnthropicSSE(context.Background(), resp, w, "claude-sonnet", "gpt-5.6-sol", original, "", "", time.Now(), 0, openAISessionResolution{})
					} else {
						outcome, err = (&OpenAIGateway{}).handleAnthropicNonStreamFromResponses(resp, nil, "claude-sonnet", "gpt-5.6-sol", original, "", "", time.Now(), openAISessionResolution{}, 0)
					}
					if err != nil || outcome.Kind != sdk.OutcomeSuccess {
						t.Fatalf("Anthropic outcome=%+v err=%v", outcome, err)
					}
					assertNoInferredCreation(t, outcome.Usage, cached)
				})
			}
		})
	}
}

func receiveNativeUsageFixture(t *testing.T, event string) WSResult {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	return ReceiveWSResponse(context.Background(), conn, nil)
}

func assertNoInferredCreation(t *testing.T, usage *sdk.Usage, cached int) {
	t.Helper()
	if usage == nil || usage.InputTokens != 1000-cached || usage.CachedInputTokens != cached || usage.OutputTokens != 50 || usage.CacheCreationTokens != 0 || usage.CacheCreationCost != 0 {
		t.Fatalf("inferred cache creation or wrong buckets: %+v", usage)
	}
	if usageMetricInt(usage, usageMetricTotalTokens) != 1050 {
		t.Fatalf("incorrect total: %+v", usage)
	}
	spec := model.Lookup("gpt-5.6-sol")
	want := (float64(1000-cached)*spec.InputPrice + float64(cached)*spec.CachedPrice + 50*spec.OutputPrice) / 1e6
	if math.Abs(usage.AccountCost-want) > 1e-12 {
		t.Fatalf("cost=%g want=%g; no cache creation charge allowed", usage.AccountCost, want)
	}
}
