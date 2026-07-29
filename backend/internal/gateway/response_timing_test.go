package gateway

import (
	"testing"
	"time"
)

func TestResponseEventTimingRecordsReasoningItemAsFirstToken(t *testing.T) {
	timing := newResponseEventTiming(time.Now().Add(-time.Second))
	timing.observe("response.created", []byte(`{"type":"response.created"}`))
	if timing.firstTokenRecorded {
		t.Fatal("control event must not record first token")
	}

	timing.observe("response.output_item.added", []byte(`{"type":"response.output_item.added","item":{"type":"reasoning","summary":[]}}`))
	if !timing.firstTokenRecorded || timing.firstTokenMs <= 0 {
		t.Fatalf("reasoning item did not record first token: recorded=%v ms=%d", timing.firstTokenRecorded, timing.firstTokenMs)
	}
}

func TestIsChatCompletionsOutputIncludesReasoningAliases(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"choices":[{"delta":{"reasoning":"think"}}]}`),
		[]byte(`{"choices":[{"delta":{"thinking":"think"}}]}`),
		[]byte(`{"choices":[{"message":{"reasoning_content":"think"}}]}`),
	} {
		if !isChatCompletionsOutput(data) {
			t.Fatalf("reasoning output was not detected: %s", data)
		}
	}
}

func TestStreamDataHasOutputIncludesReasoningItem(t *testing.T) {
	data := `{"type":"response.output_item.added","item":{"type":"reasoning","summary":[]}}`
	if !streamDataHasOutput(data) {
		t.Fatal("reasoning item must be treated as stream output")
	}
}
