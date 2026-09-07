package gateway

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type responseFillReader struct{}

func (responseFillReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestResponseBodyLimitIncludesDecodedGzip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		_, _ = io.CopyN(compressed, responseFillReader{}, sdk.MaxBufferedResponseBytes+1)
		_ = compressed.Close()
	}))
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if !response.Uncompressed {
		t.Fatal("fixture was not automatically decoded")
	}
	if data, err := readResponseBody(response); !errors.Is(err, errResponseTooLarge) || data != nil {
		t.Fatal("decompressed response exceeded budget")
	}
}

func TestSSEAggregateAndControlBudgets(t *testing.T) {
	delta := `data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("a", 8192) + `"}` + "\n\n"
	result := ParseSSEStream(io.LimitReader(&repeatedEventReader{event: []byte(delta)}, 12<<20), nil)
	if !errors.Is(result.Err, errResponseTooLarge) || len(result.Text) > 8<<20 {
		t.Fatal("cumulative text escaped budget")
	}
	w := httptest.NewRecorder()
	handler := &sseEventWriter{w: w}
	data := []byte(`{"type":"response.in_progress","pad":"` + strings.Repeat("x", 8192) + `"}`)
	for range 200 {
		handler.OnRawEvent("response.in_progress", data)
	}
	if !errors.Is(handler.Err(), errResponseTooLarge) || len(handler.pendingEvents) != 0 || w.Body.Len() != 0 {
		t.Fatal("control events escaped pending budget")
	}
	// A large first real output is not part of the small control-only buffer.
	handler = &sseEventWriter{w: httptest.NewRecorder()}
	handler.OnRawEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"`+strings.Repeat("x", 2<<20)+`"}`))
	if handler.Err() != nil || !handler.wrote {
		t.Fatal("valid first output was treated as control data")
	}
}

type repeatedEventReader struct {
	event  []byte
	offset int
}

func (r *repeatedEventReader) Read(p []byte) (int, error) {
	n := copy(p, r.event[r.offset:])
	r.offset = (r.offset + n) % len(r.event)
	return n, nil
}

func TestGatewayOversizedResponseIsTerminal(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.CopyN(w, responseFillReader{}, sdk.MaxBufferedResponseBytes+1)
	}))
	defer server.Close()
	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), transportPool: NewTransportPool()}
	defer g.transportPool.CloseIdle()
	outcome, err := g.Forward(context.Background(), &sdk.ForwardRequest{Account: &sdk.Account{ID: 321, Type: "apikey", Platform: "openai", Credentials: map[string]string{"api_key": "test", "base_url": server.URL}}, Model: "gpt-5", Body: []byte(`{"model":"gpt-5","messages":[]}`), Headers: http.Header{"X-Forwarded-Path": {"/v1/chat/completions"}, "X-Forwarded-Method": {"POST"}}})
	if !errors.Is(err, errResponseTooLarge) || outcome.Upstream.StatusCode != 502 || outcome.FailoverScope != sdk.FailoverScopeTerminal || requests.Load() != 1 {
		t.Fatalf("oversized response = %v, status=%d scope=%s requests=%d", err, outcome.Upstream.StatusCode, outcome.FailoverScope, requests.Load())
	}
}
