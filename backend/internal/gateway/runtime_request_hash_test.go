package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestContextWindowCacheReportsConditionWithoutPolicy(t *testing.T) {
	var hash runtimeHash
	req := &sdk.ForwardRequest{Model: "client", Body: []byte(`{"model":"client","input":"hello"}`), Headers: http.Header{}, DispatchPlan: sdk.DispatchPlan{ClientModel: "client", WireModel: "small"}}
	ctx := context.Background()
	first, begin := hash.BeginRequest(ctx, req, http.MethodPost, "/v1/responses")
	if begin.Outcome != nil {
		t.Fatal("unexpected cache hit")
	}
	failure := sdk.ForwardOutcome{Kind: sdk.OutcomeClientError, Upstream: sdk.UpstreamResponse{StatusCode: 400, Body: []byte(`{"error":{"code":"context_too_large","message":"Your input exceeds the context window."}}`)}}
	if finish := first.Finish(failure, nil); !finish.ContextWindowCached {
		t.Fatal("overflow was not cached")
	}
	_, begin = hash.BeginRequest(ctx, req, http.MethodPost, "/v1/responses")
	if begin.Outcome == nil || !begin.Outcome.RequestsModelFallback() {
		t.Fatalf("missing condition: %+v", begin)
	}
	// Even a formerly-default long model is just another model to the plugin.
	req.DispatchPlan.WireModel = "gpt-5.6-sol"
	second, begin := hash.BeginRequest(ctx, req, http.MethodPost, "/v1/responses")
	if begin.Outcome != nil {
		t.Fatal("other wire model inherited cached overflow")
	}
	second.Finish(failure, nil)
	_, begin = hash.BeginRequest(ctx, req, http.MethodPost, "/v1/responses")
	if begin.Outcome == nil || !begin.Outcome.RequestsModelFallback() {
		t.Fatal("plugin must report overflow regardless of Core policy")
	}
}

func TestForwardContextOverflowCachesPerWireModel(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"invalid_request_error\",\"code\":\"context_too_large\",\"message\":\"Your input exceeds the context window.\"}}}\n\n")
	}))
	defer server.Close()
	gateway := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), transportPool: NewTransportPool()}
	defer gateway.transportPool.CloseIdle()
	makeRequest := func(wire string) *sdk.ForwardRequest {
		return &sdk.ForwardRequest{
			Account: &sdk.Account{ID: 1, Credentials: map[string]string{"api_key": "test", "base_url": server.URL}},
			Model:   "client", Body: []byte(`{"model":"client","input":"hello","stream":true}`),
			Headers:      http.Header{"X-Forwarded-Path": {"/v1/responses"}, "Content-Type": {"application/json"}},
			DispatchPlan: sdk.DispatchPlan{ClientModel: "client", WireModel: wire}, Stream: true, Writer: httptest.NewRecorder(),
		}
	}
	ctx := sdk.WithLogger(context.Background(), gateway.logger)
	for i, wire := range []string{"small", "small", "large", "large"} {
		req := makeRequest(wire)
		out, err := gateway.Forward(ctx, req)
		if i%2 == 0 {
			if !isContextWindowExceededForwardResult(out, err) || out.RequestsModelFallback() {
				t.Fatalf("first attempt should return real upstream failure: %+v / %v", out, err)
			}
		} else {
			if err != nil || !out.RequestsModelFallback() {
				t.Fatalf("repeat should report cached overflow: %+v / %v", out, err)
			}
			if req.Writer.(*httptest.ResponseRecorder).Body.Len() != 0 {
				t.Fatal("control outcome committed a stream")
			}
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("upstream attempts=%d want 2", attempts.Load())
	}
}

func TestContextWindowSignalUsesAnthropicErrorShape(t *testing.T) {
	out := contextWindowRerouteOutcome(true)
	if !out.RequestsModelFallback() || gjson.GetBytes(out.Upstream.Body, "type").String() != "error" {
		t.Fatalf("bad condition: %+v", out)
	}
}
func TestRequestRetryCacheDefaults(t *testing.T) {
	if requestRetryCacheTTL != 24*time.Hour {
		t.Fatalf("retry cache TTL = %s, want 24h", requestRetryCacheTTL)
	}
	if requestRetryCacheMaxEntries != 100_000 {
		t.Fatalf("retry cache capacity = %d, want 100000", requestRetryCacheMaxEntries)
	}
}

func TestRequestRetryCacheCapacity(t *testing.T) {
	hashes := make([]uint64, requestRetryCacheMaxEntries+1)
	for index := range hashes {
		hashes[index] = uint64(index + 1)
	}
	var cache safetyRequestCache
	now := time.Now()
	cache.addHashesWithLimits(
		hashes,
		now,
		requestRetryCacheTTL,
		requestRetryCacheMaxEntries,
	)
	size, capacity := cache.statsWithCapacity(now, requestRetryCacheMaxEntries)
	if capacity != requestRetryCacheMaxEntries {
		t.Fatalf("retry cache capacity = %d, want %d", capacity, requestRetryCacheMaxEntries)
	}
	if size != requestRetryCacheMaxEntries {
		t.Fatalf("retry cache size = %d, want capped at %d", size, requestRetryCacheMaxEntries)
	}
}
