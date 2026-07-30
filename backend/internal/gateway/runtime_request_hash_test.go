package gateway

import (
	"context"
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

func TestContextWindowRerouteReusesTextRequestHash(t *testing.T) {
	t.Setenv(longContextModelEnv, "gpt-long")
	gateway := &OpenAIGateway{}
	newRequest := func(model string) *sdk.ForwardRequest {
		return &sdk.ForwardRequest{
			Body:    []byte(`{"model":"gpt-short","stream":true,"input":"compress this conversation"}`),
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Model:   "gpt-short",
			DispatchPlan: sdk.DispatchPlan{
				ClientModel:     model,
				SchedulingModel: model,
				WireModel:       model,
			},
			Stream: true,
		}
	}

	firstReq := newRequest("gpt-short")
	ctx, safetyOutcome := gateway.checkTextSafetyRequest(context.Background(), firstReq, http.MethodPost, "/v1/responses")
	if safetyOutcome != nil {
		t.Fatal("uncached request should not hit text safety cache")
	}
	firstState, reroute := gateway.checkContextWindowReroute(ctx, firstReq, "/v1/responses")
	if reroute != nil || firstState.cached || !firstState.hashReady {
		t.Fatalf("first context-window state = %+v, reroute=%+v", firstState, reroute)
	}
	if !gateway.cacheContextWindowExceeded(firstState) {
		t.Fatal("context-too-large result should cache the existing text request hash")
	}

	secondReq := newRequest("gpt-short")
	secondCtx, safetyOutcome := gateway.checkTextSafetyRequest(context.Background(), secondReq, http.MethodPost, "/v1/responses")
	if safetyOutcome != nil {
		t.Fatal("context-window marker must not be treated as a text safety rejection")
	}
	secondState, reroute := gateway.checkContextWindowReroute(secondCtx, secondReq, "/v1/responses")
	if reroute == nil || !secondState.cached || secondState.hash != firstState.hash {
		t.Fatalf("second context-window state = %+v, reroute=%+v", secondState, reroute)
	}
	if reroute.FailoverScope != sdk.FailoverScopeModelReroute || reroute.RerouteClientModel != "gpt-long" {
		t.Fatalf("reroute outcome = %+v", reroute)
	}
	if reroute.Kind != sdk.OutcomeClientError || reroute.Upstream.StatusCode != http.StatusBadRequest {
		t.Fatalf("reroute control outcome = %+v", reroute)
	}
	if code := gjson.GetBytes(reroute.Upstream.Body, "error.code").String(); code != "context_too_large" {
		t.Fatalf("reroute control code = %q", code)
	}

	longContextReq := newRequest("gpt-long")
	longContextReq.DispatchPlan.SchedulingModel = "long-context-pool"
	longContextReq.DispatchPlan.WireModel = "openai-internal-long-context"
	longContextCtx, _ := gateway.checkTextSafetyRequest(context.Background(), longContextReq, http.MethodPost, "/v1/responses")
	longContextState, reroute := gateway.checkContextWindowReroute(longContextCtx, longContextReq, "/v1/responses")
	if reroute != nil || !longContextState.cached {
		t.Fatalf("long-context state = %+v, reroute=%+v", longContextState, reroute)
	}
	if gateway.cacheContextWindowExceeded(longContextState) {
		t.Fatal("failed long-context attempt must not add a second request hash")
	}

	size, capacity := requestRetryCacheForTest(gateway).statsWithCapacity(time.Now(), requestRetryCacheMaxEntries)
	if size != 1 || capacity != requestRetryCacheMaxEntries {
		t.Fatalf("request retry cache = %d/%d, want 1/%d", size, capacity, requestRetryCacheMaxEntries)
	}
}

func TestContextWindowRerouteModelMappingMatrix(t *testing.T) {
	testCases := []struct {
		name                   string
		configuredLongModel    string
		requestModel           string
		dispatchClientModel    string
		schedulingModel        string
		wireModel              string
		wantRerouteClientModel string
	}{
		{
			name:                   "short client model mapped to long model wire still reroutes",
			configuredLongModel:    "router@openai/gpt-long",
			requestModel:           "gpt-short",
			dispatchClientModel:    "gpt-short",
			schedulingModel:        "short-pool",
			wireModel:              "gpt-long",
			wantRerouteClientModel: "gpt-long",
		},
		{
			name:                "rerouted long model mapped to different wire does not loop",
			configuredLongModel: "gpt-long",
			requestModel:        "gpt-short",
			dispatchClientModel: "gpt-long",
			schedulingModel:     "long-pool",
			wireModel:           "internal-long-wire",
		},
		{
			name:                "client directly requests long model",
			configuredLongModel: "gpt-long",
			requestModel:        "gpt-long",
			dispatchClientModel: "gpt-long",
			schedulingModel:     "gpt-long",
			wireModel:           "gpt-long",
		},
		{
			name:                "long model aliases are equivalent",
			configuredLongModel: "openai/gpt-long",
			requestModel:        "oai/gpt-long",
			dispatchClientModel: "OPENAI/GPT-LONG",
			schedulingModel:     "long-pool",
			wireModel:           "internal-long-wire",
		},
		{
			name:                "missing dispatch client model falls back to request model",
			configuredLongModel: "gpt-long",
			requestModel:        "openai/gpt-long",
			schedulingModel:     "long-pool",
			wireModel:           "internal-long-wire",
		},
		{
			name:                   "changing configured long model reroutes the previous default",
			configuredLongModel:    "openai/gpt-next-long",
			requestModel:           defaultLongContextModel,
			dispatchClientModel:    defaultLongContextModel,
			schedulingModel:        defaultLongContextModel,
			wireModel:              defaultLongContextModel,
			wantRerouteClientModel: "gpt-next-long",
		},
		{
			name:                "replacement long model mapping does not loop",
			configuredLongModel: "openai/gpt-next-long",
			requestModel:        defaultLongContextModel,
			dispatchClientModel: "oai/gpt-next-long",
			schedulingModel:     "next-long-pool",
			wireModel:           "next-long-wire",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(longContextModelEnv, testCase.configuredLongModel)
			gateway := &OpenAIGateway{}
			req := &sdk.ForwardRequest{
				Body:    []byte(`{"model":"client-visible","input":"same request"}`),
				Headers: http.Header{"Content-Type": []string{"application/json"}},
				Model:   testCase.requestModel,
				DispatchPlan: sdk.DispatchPlan{
					ClientModel:     testCase.dispatchClientModel,
					SchedulingModel: testCase.schedulingModel,
					WireModel:       testCase.wireModel,
				},
			}
			ctx, safetyOutcome := gateway.checkTextSafetyRequest(context.Background(), req, http.MethodPost, "/v1/responses")
			if safetyOutcome != nil {
				t.Fatal("mapping test unexpectedly hit text safety cache")
			}
			hash, ok := textRequestHashFromContext(ctx)
			if !ok {
				t.Fatal("text request hash was not captured")
			}
			requestRetryCacheForTest(gateway).addHashesWithLimits(
				[]uint64{hash},
				time.Now(),
				requestRetryCacheTTL,
				requestRetryCacheMaxEntries,
			)

			state, outcome := gateway.checkContextWindowReroute(ctx, req, "/v1/responses")
			if state.longContextModel != normalizeModelID(testCase.configuredLongModel, defaultLongContextModel) {
				t.Fatalf("long context model = %q", state.longContextModel)
			}
			if testCase.wantRerouteClientModel == "" {
				if outcome != nil {
					t.Fatalf("unexpected reroute outcome: %+v", outcome)
				}
				return
			}
			if outcome == nil {
				t.Fatalf("expected reroute to %q", testCase.wantRerouteClientModel)
			}
			if target, ok := outcome.ModelRerouteClientTarget(); !ok || target != testCase.wantRerouteClientModel {
				t.Fatalf("reroute target = %q, ok=%v, outcome=%+v", target, ok, outcome)
			}
		})
	}
}

func TestContextWindowExceededCachesOnlyNonLongContextModels(t *testing.T) {
	testCases := []struct {
		name                string
		configuredLongModel string
		requestModel        string
		dispatchClientModel string
		schedulingModel     string
		wireModel           string
		wantCached          bool
	}{
		{
			name:                "short model is cached even when mapped to long wire",
			configuredLongModel: "gpt-long",
			requestModel:        "gpt-short",
			dispatchClientModel: "gpt-short",
			schedulingModel:     "short-pool",
			wireModel:           "gpt-long",
			wantCached:          true,
		},
		{
			name:                "direct long model is not cached",
			configuredLongModel: "gpt-long",
			requestModel:        "gpt-long",
			dispatchClientModel: "gpt-long",
			schedulingModel:     "long-pool",
			wireModel:           "internal-long-wire",
		},
		{
			name:                "long model alias is not cached",
			configuredLongModel: "openai/gpt-long",
			requestModel:        "oai/gpt-long",
			dispatchClientModel: "openai/gpt-long",
			schedulingModel:     "long-pool",
			wireModel:           "internal-long-wire",
		},
		{
			name:                "missing dispatch client uses direct request model",
			configuredLongModel: "gpt-long",
			requestModel:        "openai/gpt-long",
			schedulingModel:     "long-pool",
			wireModel:           "internal-long-wire",
		},
		{
			name:                "old default becomes cacheable after configured replacement",
			configuredLongModel: "gpt-next-long",
			requestModel:        defaultLongContextModel,
			dispatchClientModel: defaultLongContextModel,
			schedulingModel:     defaultLongContextModel,
			wireModel:           defaultLongContextModel,
			wantCached:          true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(longContextModelEnv, testCase.configuredLongModel)
			gateway := &OpenAIGateway{}
			req := &sdk.ForwardRequest{
				Body:    []byte(`{"model":"client-visible","input":"same request"}`),
				Headers: http.Header{"Content-Type": []string{"application/json"}},
				Model:   testCase.requestModel,
				DispatchPlan: sdk.DispatchPlan{
					ClientModel:     testCase.dispatchClientModel,
					SchedulingModel: testCase.schedulingModel,
					WireModel:       testCase.wireModel,
				},
			}
			ctx, _ := gateway.checkTextSafetyRequest(context.Background(), req, http.MethodPost, "/v1/responses")
			state, outcome := gateway.checkContextWindowReroute(ctx, req, "/v1/responses")
			if outcome != nil {
				t.Fatalf("uncached first request unexpectedly requested reroute: %+v", outcome)
			}
			if cached := gateway.cacheContextWindowExceeded(state); cached != testCase.wantCached {
				t.Fatalf("cache result = %v, want %v; state=%+v", cached, testCase.wantCached, state)
			}
			size, _ := requestRetryCacheForTest(gateway).statsWithCapacity(time.Now(), requestRetryCacheMaxEntries)
			wantSize := 0
			if testCase.wantCached {
				wantSize = 1
			}
			if size != wantSize {
				t.Fatalf("request retry cache size = %d, want %d", size, wantSize)
			}
		})
	}
}

func TestContextWindowRerouteUsesAnthropicErrorShape(t *testing.T) {
	t.Setenv(longContextModelEnv, "gpt-long")
	gateway := &OpenAIGateway{}
	newRequest := func() *sdk.ForwardRequest {
		return &sdk.ForwardRequest{
			Body: []byte(`{"model":"claude-sonnet","max_tokens":1024,"messages":[{"role":"user","content":"long conversation"}]}`),
			Headers: http.Header{
				"Content-Type":      []string{"application/json"},
				"Anthropic-Version": []string{"2023-06-01"},
			},
			Model: "claude-sonnet",
			DispatchPlan: sdk.DispatchPlan{
				ClientModel:     "claude-sonnet",
				SchedulingModel: "gpt-short",
				WireModel:       "gpt-short",
			},
		}
	}

	firstReq := newRequest()
	ctx, cachedSafety := gateway.checkTextSafetyRequest(context.Background(), firstReq, http.MethodPost, "/v1/messages")
	if cachedSafety != nil {
		t.Fatal("first Anthropic request unexpectedly hit text safety cache")
	}
	state, reroute := gateway.checkContextWindowReroute(ctx, firstReq, "/v1/messages")
	if reroute != nil || !gateway.cacheContextWindowExceeded(state) {
		t.Fatalf("first Anthropic context-window state=%+v reroute=%+v", state, reroute)
	}

	retryReq := newRequest()
	retryCtx, _ := gateway.checkTextSafetyRequest(context.Background(), retryReq, http.MethodPost, "/v1/messages")
	_, reroute = gateway.checkContextWindowReroute(retryCtx, retryReq, "/v1/messages")
	if reroute == nil {
		t.Fatal("second Anthropic request should request model reroute")
	}
	if target, ok := reroute.ModelRerouteClientTarget(); !ok || target != "gpt-long" {
		t.Fatalf("Anthropic reroute outcome = %+v", reroute)
	}
	if got := gjson.GetBytes(reroute.Upstream.Body, "type").String(); got != "error" {
		t.Fatalf("Anthropic response type = %q, body=%s", got, reroute.Upstream.Body)
	}
	if got := gjson.GetBytes(reroute.Upstream.Body, "error.code").String(); got != "context_too_large" {
		t.Fatalf("Anthropic error code = %q, body=%s", got, reroute.Upstream.Body)
	}
}

func TestForwardStreamingContextWindowReroutesOnlyOnNextClientRequest(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attempt := requestCount.Add(1)
		wantModel := "short-wire-model"
		if attempt == 2 {
			wantModel = "long-wire-model"
		}
		if got := gjson.GetBytes(body, "model").String(); got != wantModel {
			t.Errorf("upstream attempt %d model = %q, want %q; body=%s", attempt, got, wantModel, body)
		}
		if attempt > 2 {
			t.Errorf("unexpected upstream attempt %d", attempt)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"context_too_large","message":"Your input exceeds the context window."}}}`+"\n\n")
	}))
	defer server.Close()

	newRequest := func(dispatchClientModel, schedulingModel, wireModel string) *sdk.ForwardRequest {
		return &sdk.ForwardRequest{
			Account: &sdk.Account{ID: 1, Credentials: map[string]string{
				"api_key":  "sk-test",
				"base_url": server.URL,
			}},
			Body: []byte(`{"model":"gpt-short","stream":true,"input":"compress this conversation"}`),
			Headers: http.Header{
				"Content-Type":     []string{"application/json"},
				"X-Forwarded-Path": []string{"/v1/responses"},
			},
			Model: "gpt-short",
			DispatchPlan: sdk.DispatchPlan{
				ClientModel:     dispatchClientModel,
				SchedulingModel: schedulingModel,
				WireModel:       wireModel,
			},
			Stream: true,
			Writer: httptest.NewRecorder(),
		}
	}

	t.Setenv(longContextModelEnv, "gpt-long")
	gateway := &OpenAIGateway{
		logger:        slog.Default(),
		transportPool: NewTransportPool(),
	}
	ctx := sdk.WithLogger(context.Background(), slog.Default())

	firstOutcome, firstErr := gateway.Forward(ctx, newRequest("gpt-short", "short-context-pool", "short-wire-model"))
	if !isContextWindowExceededForwardResult(firstOutcome, firstErr) {
		t.Fatalf("first request outcome=%+v err=%v, want context_too_large", firstOutcome, firstErr)
	}
	if firstOutcome.FailoverScope == sdk.FailoverScopeModelReroute {
		t.Fatal("first context-too-large response must be returned to the client without transparent reroute")
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("first client request made %d upstream attempts, want 1", got)
	}

	secondReq := newRequest("gpt-short", "short-context-pool", "short-wire-model")
	secondOutcome, secondErr := gateway.Forward(ctx, secondReq)
	if secondErr != nil {
		t.Fatalf("second client request returned plugin error: %v", secondErr)
	}
	if target, ok := secondOutcome.ModelRerouteClientTarget(); !ok || target != "gpt-long" {
		t.Fatalf("second client request outcome=%+v, want gpt-long reroute", secondOutcome)
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("reroute control request made %d upstream attempts, want count to remain 1", got)
	}
	if recorder := secondReq.Writer.(*httptest.ResponseRecorder); recorder.Body.Len() != 0 {
		t.Fatalf("reroute control request wrote streaming output: %q", recorder.Body.String())
	}

	longContextOutcome, longContextErr := gateway.Forward(ctx, newRequest("gpt-long", "long-context-pool", "long-wire-model"))
	if !isContextWindowExceededForwardResult(longContextOutcome, longContextErr) {
		t.Fatalf("long-context request outcome=%+v err=%v, want context_too_large", longContextOutcome, longContextErr)
	}
	if _, ok := longContextOutcome.ModelRerouteClientTarget(); ok || longContextOutcome.FailoverScope == sdk.FailoverScopeModelReroute {
		t.Fatalf("failed long-context request requested another reroute: %+v", longContextOutcome)
	}
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("full client retry flow made %d upstream attempts, want exactly 2", got)
	}

	size, capacity := requestRetryCacheForTest(gateway).statsWithCapacity(time.Now(), requestRetryCacheMaxEntries)
	if size != 1 || capacity != requestRetryCacheMaxEntries {
		t.Fatalf("request retry cache = %d/%d, want 1/%d", size, capacity, requestRetryCacheMaxEntries)
	}
}

func TestForwardDirectLongContextModelNeverEntersRerouteCache(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestCount.Add(1)
		if got := gjson.GetBytes(body, "model").String(); got != "long-wire-model" {
			t.Errorf("upstream model = %q, want long-wire-model; body=%s", got, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"context_too_large","message":"Your input exceeds the context window."}}`)
	}))
	defer server.Close()

	newRequest := func() *sdk.ForwardRequest {
		return &sdk.ForwardRequest{
			Account: &sdk.Account{ID: 1, Credentials: map[string]string{
				"api_key":  "sk-test",
				"base_url": server.URL,
			}},
			Body: []byte(`{"model":"gpt-long","input":"already using the long model"}`),
			Headers: http.Header{
				"Content-Type":     []string{"application/json"},
				"X-Forwarded-Path": []string{"/v1/responses"},
			},
			Model: "gpt-long",
			DispatchPlan: sdk.DispatchPlan{
				ClientModel:     "gpt-long",
				SchedulingModel: "long-context-pool",
				WireModel:       "long-wire-model",
			},
		}
	}

	t.Setenv(longContextModelEnv, "gpt-long")
	gateway := &OpenAIGateway{
		logger:        slog.Default(),
		transportPool: NewTransportPool(),
	}
	ctx := sdk.WithLogger(context.Background(), slog.Default())
	for attempt := 1; attempt <= 2; attempt++ {
		outcome, err := gateway.Forward(ctx, newRequest())
		if !isContextWindowExceededForwardResult(outcome, err) {
			t.Fatalf("direct long-context attempt %d outcome=%+v err=%v", attempt, outcome, err)
		}
		if _, ok := outcome.ModelRerouteClientTarget(); ok || outcome.FailoverScope == sdk.FailoverScopeModelReroute {
			t.Fatalf("direct long-context attempt %d requested reroute: %+v", attempt, outcome)
		}
		size, _ := requestRetryCacheForTest(gateway).statsWithCapacity(time.Now(), requestRetryCacheMaxEntries)
		if size != 0 {
			t.Fatalf("direct long-context attempt %d populated reroute cache: size=%d", attempt, size)
		}
	}
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("two direct long-context client requests made %d upstream attempts, want 2", got)
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
