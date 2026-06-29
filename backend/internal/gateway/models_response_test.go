package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestEnrichModelsResponse_AddsContextMetadata(t *testing.T) {
	upstream := `{"object":"list","data":[{"id":"gpt-4o","object":"model"},{"id":"o3","object":"model","context_window":200000}]}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       ioNopCloserFromString(upstream),
	}

	out := enrichModelsResponse(resp)
	if out == nil || out.Body == nil {
		t.Fatalf("enrichModelsResponse returned nil body")
	}

	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(out.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Data) != 2 {
		t.Fatalf("models count = %d, want 2", len(payload.Data))
	}

	m0 := payload.Data[0]
	if toInt(m0["context_window"]) != 128000 {
		t.Fatalf("gpt-4o context_window = %v, want 128000", m0["context_window"])
	}
	if toInt(m0["max_input_tokens"]) != 128000 {
		t.Fatalf("gpt-4o max_input_tokens = %v, want 128000", m0["max_input_tokens"])
	}
	if toInt(m0["max_output_tokens"]) != 16384 {
		t.Fatalf("gpt-4o max_output_tokens = %v, want 16384", m0["max_output_tokens"])
	}

	m1 := payload.Data[1]
	if toInt(m1["context_window"]) != 200000 {
		t.Fatalf("o3 context_window = %v, want 200000", m1["context_window"])
	}
}

func ioNopCloserFromString(s string) *readCloser {
	return &readCloser{r: strings.NewReader(s)}
}

type readCloser struct{ r *strings.Reader }

func (c *readCloser) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *readCloser) Close() error               { return nil }

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}
