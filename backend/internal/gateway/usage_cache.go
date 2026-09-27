package gateway

// cacheCreationTokensFromFields resolves alternative representations of the same
// cache-write measurement. Never add aliases or add a TTL breakdown to its total.
// An explicitly reported zero is authoritative. Only absent totals permit the
// two disjoint TTL buckets to be summed. Both Responses parsers use this policy.
// These tokens are a subset of OpenAI input_tokens; splitInputTokenBuckets must
// subtract them before the SDK receives exclusive ordinary/read/write buckets.
// A mode switch never creates a measurement: if upstream omits all creation
// fields, return zero. Uncached input is not evidence of a cache write.
func cacheCreationTokensFromFields(read func(string) (int, bool)) int {
	for _, path := range []string{
		"input_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_write_tokens",
		"input_tokens_details.cache_creation_tokens",
		"prompt_tokens_details.cache_creation_tokens",
		"cache_creation_input_tokens",
		"cache_write_input_tokens",
		"cache_write_tokens",
		"cache_creation_tokens",
	} {
		if value, exists := read(path); exists {
			return max(value, 0)
		}
	}
	fiveMinutes, _ := read("cache_creation.ephemeral_5m_input_tokens")
	oneHour, _ := read("cache_creation.ephemeral_1h_input_tokens")
	fiveMinutes, oneHour = max(fiveMinutes, 0), max(oneHour, 0)
	maxInt := int(^uint(0) >> 1)
	if fiveMinutes > maxInt-oneHour {
		return maxInt
	}
	return fiveMinutes + oneHour
}
