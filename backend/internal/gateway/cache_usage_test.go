package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestSplitInputTokenBuckets(t *testing.T) {
	tests := []struct {
		name                               string
		raw, cached, created               int
		wantInput, wantCached, wantCreated int
	}{
		{name: "plain input", raw: 100, wantInput: 100},
		{name: "cache read only", raw: 100, cached: 20, wantInput: 80, wantCached: 20},
		{name: "cache write only", raw: 100, created: 30, wantInput: 70, wantCreated: 30},
		{name: "read and write", raw: 100, cached: 20, created: 30, wantInput: 50, wantCached: 20, wantCreated: 30},
		{name: "clamp invalid components", raw: 100, cached: 80, created: 50, wantCached: 80, wantCreated: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, cached, created := splitInputTokenBuckets(tt.raw, tt.cached, tt.created)
			if input != tt.wantInput || cached != tt.wantCached || created != tt.wantCreated {
				t.Fatalf("split = %d/%d/%d, want %d/%d/%d", input, cached, created, tt.wantInput, tt.wantCached, tt.wantCreated)
			}
			if input+cached+created != tt.raw {
				t.Fatalf("split total = %d, want raw input %d", input+cached+created, tt.raw)
			}
		})
	}
}

func TestParseUsageCacheWriteTokens(t *testing.T) {
	tests := []struct {
		name                               string
		body                               string
		wantInput, wantCached, wantCreated int
	}{
		{
			name:      "zero write keeps non-cached input normal",
			body:      `{"usage":{"input_tokens":100,"cache_creation_input_tokens":30,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":0}}}`,
			wantInput: 80, wantCached: 20,
		},
		{
			name:      "nonzero write is separated",
			body:      `{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":30}}}`,
			wantInput: 50, wantCached: 20, wantCreated: 30,
		},
		{
			name:      "missing write treats all input as normal",
			body:      `{"usage":{"input_tokens":100}}`,
			wantInput: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseUsage([]byte(tt.body))
			if got.inputTokens != tt.wantInput || got.cachedInputTokens != tt.wantCached || got.cacheCreationTokens != tt.wantCreated {
				t.Fatalf("parsed = %d/%d/%d, want %d/%d/%d", got.inputTokens, got.cachedInputTokens, got.cacheCreationTokens, tt.wantInput, tt.wantCached, tt.wantCreated)
			}
		})
	}
}

func TestParseSSEUsageCacheWriteTokens(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{
			name: "responses",
			data: `{"type":"response.completed","response":{"model":"gpt-5.6-sol","usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":30}}}}`,
		},
		{
			name: "chat completions",
			data: `{"object":"chat.completion.chunk","model":"gpt-5.6-sol","usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":20,"cache_write_tokens":30}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &sdk.Usage{}
			parseSSEUsage([]byte(tt.data), usage, nil, nil)
			if usage.InputTokens != 50 || usage.CachedInputTokens != 20 || usage.CacheCreationTokens != 30 || usage.OutputTokens != 10 {
				t.Fatalf("usage = input %d, cached %d, write %d, output %d", usage.InputTokens, usage.CachedInputTokens, usage.CacheCreationTokens, usage.OutputTokens)
			}
		})
	}
}

func TestExtractUsageFromResponseMapCacheWriteTokens(t *testing.T) {
	result := &WSResult{}
	extractUsageFromResponseMap(result, map[string]any{
		"usage": map[string]any{
			"input_tokens":  float64(100),
			"output_tokens": float64(10),
			"input_tokens_details": map[string]any{
				"cached_tokens":      float64(20),
				"cache_write_tokens": float64(30),
			},
		},
	})

	if result.InputTokens != 50 || result.CachedInputTokens != 20 || result.CacheCreationTokens != 30 || result.OutputTokens != 10 {
		t.Fatalf("result = input %d, cached %d, write %d, output %d", result.InputTokens, result.CachedInputTokens, result.CacheCreationTokens, result.OutputTokens)
	}

	zeroWrite := &WSResult{}
	extractUsageFromResponseMap(zeroWrite, map[string]any{
		"usage": map[string]any{
			"input_tokens":                float64(100),
			"cache_creation_input_tokens": float64(30),
			"input_tokens_details": map[string]any{
				"cache_write_tokens": float64(0),
			},
		},
	})
	if zeroWrite.InputTokens != 100 || zeroWrite.CacheCreationTokens != 0 {
		t.Fatalf("explicit zero write must be authoritative, got input %d, write %d", zeroWrite.InputTokens, zeroWrite.CacheCreationTokens)
	}

	zeroRootWrite := &WSResult{}
	extractUsageFromResponseMap(zeroRootWrite, map[string]any{
		"usage": map[string]any{
			"input_tokens":                float64(100),
			"cache_creation_input_tokens": float64(0),
			"cache_write_input_tokens":    float64(30),
		},
	})
	if zeroRootWrite.InputTokens != 100 || zeroRootWrite.CacheCreationTokens != 0 {
		t.Fatalf("explicit zero root write must be authoritative, got input %d, write %d", zeroRootWrite.InputTokens, zeroRootWrite.CacheCreationTokens)
	}
}

func TestExtractResponsesUsageCacheWriteTokens(t *testing.T) {
	usage := gjson.Parse(`{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":30}}`)
	input, output, cached, created, reasoning := extractResponsesUsage(usage)
	if input != 50 || cached != 20 || created != 30 || output != 10 || reasoning != 0 {
		t.Fatalf("usage = input %d, cached %d, write %d, output %d, reasoning %d", input, cached, created, output, reasoning)
	}
}

func TestFillUsageCostCacheWriteTokens(t *testing.T) {
	tests := []struct {
		name                              string
		input, cached, created            int
		wantInputCost, wantCachedCost     float64
		wantCreationCost, wantAccountCost float64
	}{
		{
			name:  "zero write uses normal input price",
			input: 80, cached: 20,
			wantInputCost: 0.0004, wantCachedCost: 0.00001, wantAccountCost: 0.00041,
		},
		{
			name:  "nonzero write uses cache creation price",
			input: 50, cached: 20, created: 30,
			wantInputCost: 0.00025, wantCachedCost: 0.00001, wantCreationCost: 0.0001875, wantAccountCost: 0.0004475,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := newTokenUsage("gpt-5.6-sol", "", tt.input, 0, tt.cached, tt.created, 0, 0)
			fillUsageCost(usage)
			if !almostEqual(usage.InputCost, tt.wantInputCost, 1e-12) ||
				!almostEqual(usage.CachedInputCost, tt.wantCachedCost, 1e-12) ||
				!almostEqual(usage.CacheCreationCost, tt.wantCreationCost, 1e-12) ||
				!almostEqual(usage.AccountCost, tt.wantAccountCost, 1e-12) {
				t.Fatalf("costs = input %v, cached %v, write %v, total %v", usage.InputCost, usage.CachedInputCost, usage.CacheCreationCost, usage.AccountCost)
			}
		})
	}
}

func TestFillUsageCostCacheWriteFallsBackToInputPrice(t *testing.T) {
	usage := newTokenUsage("gpt-5.5", "", 70, 0, 0, 30, 0, 0)
	fillUsageCost(usage)

	if usage.CacheCreationPrice != usage.InputPrice || usage.CacheCreationPrice != 5 {
		t.Fatalf("cache write price = %v, input price = %v, want both 5", usage.CacheCreationPrice, usage.InputPrice)
	}
	if !almostEqual(usage.AccountCost, 0.0005, 1e-12) {
		t.Fatalf("account cost = %v, want 0.0005", usage.AccountCost)
	}
}

func TestBuildNonStreamChatCompletionRestoresCacheWriteTokens(t *testing.T) {
	body := buildNonStreamChatCompletion(WSResult{
		InputTokens:         50,
		CachedInputTokens:   20,
		CacheCreationTokens: 30,
		OutputTokens:        10,
	}, "gpt-5.6-sol")

	if got := gjson.GetBytes(body, "usage.prompt_tokens").Int(); got != 100 {
		t.Fatalf("prompt_tokens = %d, want 100", got)
	}
	if got := gjson.GetBytes(body, "usage.prompt_tokens_details.cache_write_tokens").Int(); got != 30 {
		t.Fatalf("cache_write_tokens = %d, want 30", got)
	}
}

func TestHandleStreamResponseFailurePreservesCacheWriteUsage(t *testing.T) {
	body := `data: {"type":"response.failed","response":{"model":"gpt-5.6-sol","usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":30}},"error":{"type":"server_error","code":"server_overloaded","message":"overloaded"}}}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Usage == nil {
		t.Fatal("failure outcome must preserve upstream usage")
	}
	if outcome.Usage.InputTokens != 50 || outcome.Usage.CachedInputTokens != 20 || outcome.Usage.CacheCreationTokens != 30 || outcome.Usage.OutputTokens != 10 {
		t.Fatalf("usage = input %d, cached %d, write %d, output %d", outcome.Usage.InputTokens, outcome.Usage.CachedInputTokens, outcome.Usage.CacheCreationTokens, outcome.Usage.OutputTokens)
	}
}
