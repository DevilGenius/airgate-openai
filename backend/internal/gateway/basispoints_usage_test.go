package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/DevilGenius/airgate-openai/backend/internal/model"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestBasispointsCacheCreationIsExclusiveAcrossProtocols(t *testing.T) {
	fixtures := map[string]map[string]int{}
	fixtures["missing creation"] = nil
	fixtures["explicit zero creation"] = map[string]int{"input_tokens_details.cache_write_tokens": 0}
	for _, alias := range cacheWriteAliases {
		fixtures[alias] = map[string]int{alias: 300}
	}
	fixtures["TTL only"] = map[string]int{"cache_creation.ephemeral_5m_input_tokens": 100, "cache_creation.ephemeral_1h_input_tokens": 200}
	all := map[string]int{"cache_creation.ephemeral_5m_input_tokens": 100, "cache_creation.ephemeral_1h_input_tokens": 200}
	for _, alias := range cacheWriteAliases {
		all[alias] = 300
	}
	fixtures["aliases and TTL"] = all
	for name, fields := range fixtures {
		wantWrite := 300
		if name == "missing creation" || name == "explicit zero creation" {
			wantWrite = 0
		}
		wantInput := 800 - wantWrite
		for _, protocol := range []string{"responses", "chat", "anthropic"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", name, protocol, stream), func(t *testing.T) {
					completed := strings.TrimSpace(strings.TrimPrefix(bpsCompleted, "data: "))
					completed, err := sjson.SetRaw(completed, "response.usage", string(cacheUsageFixture(t, fields)))
					if err != nil {
						t.Fatal(err)
					}
					// Final usage is a snapshot, not an increment on earlier measurements.
					created, _ := json.Marshal(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_bps", "model": "gpt-5.6-sol", "status": "in_progress", "usage": map[string]any{"input_tokens": 700, "input_tokens_details": map[string]any{"cache_write_tokens": 200}}}})
					wire := "data: " + string(created) + "\n\n" + "data: " + completed + "\n\n"
					req := bpsRequest()
					req.Stream = stream
					w := httptest.NewRecorder()
					req.Writer = w
					responsesBody := req.Body
					if protocol != "responses" {
						if protocol == "chat" {
							req.Headers.Set("X-Airgate-Request-Path", "/v1/chat/completions")
						}
						req.Body, _ = json.Marshal(map[string]any{"model": req.Model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream_options": map[string]any{"include_usage": true}})
					}
					g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, wire)
					})
					var outcome sdk.ForwardOutcome
					var used bool
					if protocol == "anthropic" {
						outcome, used = g.tryBasispointsAnthropic(context.Background(), req, responsesBody, "claude-sonnet", req.Model, time.Now(), w)
					} else {
						outcome, used, err = g.tryBasispointsOAuth(context.Background(), req)
					}
					if !used || err != nil || outcome.Kind != sdk.OutcomeSuccess {
						t.Fatalf("outcome=%+v used=%v err=%v", outcome, used, err)
					}
					u := outcome.Usage
					if u == nil || u.InputTokens != wantInput || u.CachedInputTokens != 200 || u.CacheCreationTokens != wantWrite || u.OutputTokens != 50 {
						t.Fatalf("overlapping usage: %+v", u)
					}
					if usageMetricInt(u, usageMetricTotalTokens) != 1050 {
						t.Fatalf("double counted total: %+v", u)
					}
					spec := model.Lookup(req.Model)
					writePrice := spec.CacheCreationPrice
					if writePrice <= 0 {
						writePrice = spec.InputPrice
					}
					for _, cost := range []struct {
						name      string
						got, want float64
					}{
						{"input", u.InputCost, float64(wantInput) * spec.InputPrice / 1e6},
						{"cache read", u.CachedInputCost, 200 * spec.CachedPrice / 1e6},
						{"cache creation", u.CacheCreationCost, float64(wantWrite) * writePrice / 1e6},
						{"output", u.OutputCost, 50 * spec.OutputPrice / 1e6},
						{"total", u.AccountCost, (float64(wantInput)*spec.InputPrice + 200*spec.CachedPrice + float64(wantWrite)*writePrice + 50*spec.OutputPrice) / 1e6},
					} {
						if math.Abs(cost.got-cost.want) > 1e-12 {
							t.Errorf("%s cost=%g want=%g", cost.name, cost.got, cost.want)
						}
					}
					payload := string(outcome.Upstream.Body)
					if stream {
						payload = w.Body.String()
					}
					usage := gjson.Get(payload, "usage")
					if stream {
						for _, line := range strings.Split(payload, "\n") {
							if !strings.HasPrefix(line, "data: ") {
								continue
							}
							event := gjson.Parse(strings.TrimPrefix(line, "data: "))
							if protocol == "responses" && event.Get("type").String() == "response.completed" {
								usage = event.Get("response.usage")
							}
							if protocol == "chat" && event.Get("usage").IsObject() {
								usage = event.Get("usage")
							}
							if protocol == "anthropic" && event.Get("type").String() == "message_delta" {
								usage = event.Get("usage")
							}
						}
					}
					switch protocol {
					case "responses":
						if usage.Get("input_tokens").Int() != 1000 || cacheCreationTokensFromUsage(usage) != wantWrite {
							t.Fatalf("Responses should retain inclusive usage: %s", usage.Raw)
						}
					case "chat":
						if usage.Get("prompt_tokens").Int() != 1000 || usage.Get("prompt_tokens_details.cache_write_tokens").Int() != int64(wantWrite) || usage.Get("total_tokens").Int() != 1050 {
							t.Fatalf("Chat totals overlap: %s", usage.Raw)
						}
					case "anthropic":
						if usage.Get("input_tokens").Int() != int64(wantInput) || usage.Get("cache_read_input_tokens").Int() != 200 || usage.Get("cache_creation_input_tokens").Int() != int64(wantWrite) {
							t.Fatalf("Anthropic buckets overlap: %s", usage.Raw)
						}
					}
				})
			}
		}
	}
}
