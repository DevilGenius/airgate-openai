package gateway

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var cacheWriteAliases = []string{
	"input_tokens_details.cache_write_tokens",
	"prompt_tokens_details.cache_write_tokens",
	"input_tokens_details.cache_creation_tokens",
	"prompt_tokens_details.cache_creation_tokens",
	"cache_creation_input_tokens",
	"cache_write_input_tokens",
	"cache_write_tokens",
	"cache_creation_tokens",
}

func cacheUsageFixture(t *testing.T, fields map[string]int) []byte {
	t.Helper()
	raw := []byte(`{"input_tokens":1000,"output_tokens":50,"total_tokens":1050,"input_tokens_details":{"cached_tokens":200}}`)
	for path, value := range fields {
		var err error
		raw, err = sjson.SetBytes(raw, path, value)
		if err != nil {
			t.Fatal(err)
		}
	}
	return raw
}

func TestCacheCreationParsersAgreeOnAliasesAndBounds(t *testing.T) {
	cases := []struct {
		name               string
		fields             map[string]int
		input, read, write int
	}{
		{"missing", nil, 800, 200, 0},
		{"explicit zero", map[string]int{"input_tokens_details.cache_write_tokens": 0, "cache_creation_tokens": 300, "cache_creation.ephemeral_5m_input_tokens": 300}, 800, 200, 0},
		{"negative write", map[string]int{"cache_creation_tokens": -100}, 800, 200, 0},
		{"write exceeds remaining", map[string]int{"cache_creation_tokens": 900}, 0, 200, 800},
		{"reads exceed total", map[string]int{"input_tokens_details.cached_tokens": 2000, "cache_creation_tokens": 300}, 0, 1000, 0},
		{"TTL only", map[string]int{"cache_creation.ephemeral_5m_input_tokens": 100, "cache_creation.ephemeral_1h_input_tokens": 200}, 500, 200, 300},
		{"negative TTL", map[string]int{"cache_creation.ephemeral_5m_input_tokens": -100, "cache_creation.ephemeral_1h_input_tokens": 300}, 500, 200, 300},
		{"total wins over TTL", map[string]int{"cache_creation_tokens": 300, "cache_creation.ephemeral_5m_input_tokens": 300, "cache_creation.ephemeral_1h_input_tokens": 300}, 500, 200, 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertCacheUsageParsers(t, cacheUsageFixture(t, tc.fields), tc.input, tc.read, tc.write)
		})
	}
	for _, path := range cacheWriteAliases {
		t.Run(path, func(t *testing.T) {
			assertCacheUsageParsers(t, cacheUsageFixture(t, map[string]int{path: 300}), 500, 200, 300)
		})
	}
	all := map[string]int{"cache_creation.ephemeral_5m_input_tokens": 100, "cache_creation.ephemeral_1h_input_tokens": 200}
	for _, path := range cacheWriteAliases {
		all[path] = 300
	}
	assertCacheUsageParsers(t, cacheUsageFixture(t, all), 500, 200, 300)
}

func assertCacheUsageParsers(t *testing.T, raw []byte, wantInput, wantRead, wantWrite int) {
	t.Helper()
	var usage map[string]any
	if err := json.Unmarshal(raw, &usage); err != nil {
		t.Fatal(err)
	}
	result := WSResult{CachedInputTokens: 777, CacheCreationTokens: 888}
	extractUsageFromResponseMap(&result, map[string]any{"usage": usage})
	input, _, read, write, _ := extractResponsesUsage(gjson.ParseBytes(raw))
	if result.InputTokens != wantInput || result.CachedInputTokens != wantRead || result.CacheCreationTokens != wantWrite || int(input) != wantInput || int(read) != wantRead || int(write) != wantWrite {
		t.Fatalf("map=(%d,%d,%d), json=(%d,%d,%d), want=(%d,%d,%d)", result.InputTokens, result.CachedInputTokens, result.CacheCreationTokens, input, read, write, wantInput, wantRead, wantWrite)
	}
	if result.InputTokens+result.CachedInputTokens+result.CacheCreationTokens != 1000 {
		t.Fatal("input total not conserved")
	}
	extractUsageFromResponseMap(&result, map[string]any{"usage": map[string]any{"input_tokens": float64(1000)}})
	if result.InputTokens != 1000 || result.CachedInputTokens != 0 || result.CacheCreationTokens != 0 {
		t.Fatalf("stale cache counters: %+v", result)
	}
}
