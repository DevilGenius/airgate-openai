package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestStreamBudgetAllowsBase64Encoded64MiBImage(t *testing.T) {
	image := strings.Repeat("A", base64.StdEncoding.EncodedLen(64<<20))
	event := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"img","type":"image_generation_call","result":"` + image + `"}}`)
	budget := streamResponseBudget{}
	if err := budget.take(len(event), len(event)); err != nil {
		t.Fatalf("64 MiB image after base64 encoding was rejected: %v", err)
	}
}

func TestRepeatedImageSnapshotsUseOneOutputBudget(t *testing.T) {
	image := strings.Repeat("A", 17<<20)
	item := `{"id":"img","type":"image_generation_call","result":"` + image + `"}`
	preview := `{"type":"response.image_generation_call.partial_image","item_id":"img","output_index":0,"partial_image_b64":"` + image + `"}`
	done := `{"type":"response.output_item.done","output_index":0,"item":` + item + `}`
	terminal := `{"type":"response.completed","response":{"id":"resp","output":[` + item + `]}}`
	events := []string{preview, preview, done, done, done, done, terminal}
	for _, transport := range []string{"sse", "ws"} {
		t.Run(transport, func(t *testing.T) {
			var result WSResult
			if transport == "sse" {
				result = parseOutputEventsForTest(t, transport, events, nil)
			} else {
				// Large messages require a concurrent reader; unlike small fixtures
				// they cannot all be queued into the TCP socket before receiving.
				client, peer := wsTestPair(t)
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				if err := peer.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
					t.Fatal(err)
				}
				written := make(chan error, 1)
				go func() {
					for _, event := range events {
						if err := peer.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
							written <- err
							return
						}
					}
					written <- nil
				}()
				result = ReceiveWSResponse(ctx, client, nil)
				_ = client.Close()
				if err := <-written; err != nil {
					t.Fatal(err)
				}
			}
			if result.Err != nil || len(result.ImageGenCalls) != 1 || len(result.ImageGenCalls[0].Result) != len(image) {
				t.Fatalf("repeated image snapshots were not replaced: err=%v images=%d", result.Err, len(result.ImageGenCalls))
			}
			if len(gjson.GetBytes(result.CompletedEventRaw, "response.output").Array()) != 1 {
				t.Fatal("terminal output gained duplicate images")
			}
		})
	}
}

func TestRetainedOutputBudgetIsSharedAcrossKinds(t *testing.T) {
	result := WSResult{}
	var text, reasoning strings.Builder
	delta := strings.Repeat("x", 1<<20)
	for range 48 {
		if !result.appendText(&text, delta) || !result.appendText(&reasoning, delta) {
			t.Fatal(result.Err)
		}
	}
	if result.appendText(&text, "x") || !errors.Is(result.Err, errResponseTooLarge) {
		t.Fatal("combined retained output larger than 96 MiB was accepted")
	}
	if text.Len()+reasoning.Len() != 96<<20 {
		t.Fatal("oversized delta was retained before checking the limit")
	}
}

func TestRestoredOutputMustFitFinalBody(t *testing.T) {
	payload := strings.Repeat("A", 48<<20)
	accumulator := responsesOutputAccumulator{}
	accumulator.apply("response.output_item.done", []byte(`{"item":{"id":"first","type":"image_generation_call","result":"`+payload+`"}}`))
	if accumulator.err != nil {
		t.Fatal(accumulator.err)
	}
	terminal := []byte(`{"type":"response.completed","response":{"output":[{"id":"second","type":"image_generation_call","result":"` + payload + `"}]}}`)
	if err := checkResponseEvent(terminal); err != nil {
		t.Fatal("upstream terminal fixture should fit before restoration")
	}
	accumulator.apply("response.completed", terminal)
	if !errors.Is(accumulator.err, errResponseTooLarge) {
		t.Fatal("restoring a missing image let the final body exceed 96 MiB")
	}
}

func TestReplacedImageReleasesRetainedBudget(t *testing.T) {
	result := WSResult{}
	upsertImageGenCall(&result, ImageGenCall{ID: "image", Result: strings.Repeat("A", 64<<20)})
	upsertImageGenCall(&result, ImageGenCall{ID: "image", Result: strings.Repeat("B", 16<<20)})
	var text strings.Builder
	if !result.appendText(&text, strings.Repeat("x", 48<<20)) {
		t.Fatal("obsolete image preview still consumed buffer space:", result.Err)
	}
	if len(result.ImageGenCalls) != 1 || len(result.ImageGenCalls[0].Result) != 16<<20 {
		t.Fatal("image replacement retained the previous preview")
	}
	tool := map[string]any{"id": "tool", "type": "function_call", "name": "test", "arguments": "{}"}
	appendToolUseBlock(&result, tool)
	retained := result.retainedBytes
	appendToolUseBlock(&result, tool)
	if len(result.ToolUses) != 1 || result.retainedBytes != retained {
		t.Fatal("repeated tool snapshot consumed the cache budget twice")
	}
}

func TestImagePreviewWithoutIDIsReplacedByOutputIndex(t *testing.T) {
	events := []string{
		`{"type":"response.image_generation_call.partial_image","output_index":0,"partial_image_b64":"preview"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"image","type":"image_generation_call","result":"final"}}`,
		`{"type":"response.completed","response":{"output":[]}}`,
	}
	for _, transport := range []string{"sse", "ws"} {
		result := parseOutputEventsForTest(t, transport, events, nil)
		if result.Err != nil || len(result.ImageGenCalls) != 1 || result.ImageGenCalls[0].Result != "final" {
			t.Fatalf("%s retained the obsolete image preview: err=%v images=%+v", transport, result.Err, result.ImageGenCalls)
		}
	}
}

func TestStreamTrafficAndWriterBudgets(t *testing.T) {
	block := make([]byte, 1<<20)
	budget := streamResponseBudget{}
	stream := &limitedResponseWriter{ResponseWriter: discardResponseWriter{}, stream: true}
	for range 384 {
		if err := budget.take(len(block), len(block)); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if err := budget.take(1, 1); !errors.Is(err, errResponseTooLarge) {
		t.Fatal("incoming traffic escaped its budget")
	}
	if _, err := stream.Write([]byte{1}); !errors.Is(err, errResponseTooLarge) {
		t.Fatal("outgoing traffic escaped its independent budget")
	}
	buffered := &limitedResponseWriter{ResponseWriter: discardResponseWriter{}}
	for range 96 {
		if _, err := buffered.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := buffered.Write([]byte{1}); !errors.Is(err, errResponseTooLarge) {
		t.Fatal("non-stream writer used the larger stream budget")
	}
}

type discardResponseWriter struct{}

func (discardResponseWriter) Header() http.Header         { return http.Header{} }
func (discardResponseWriter) WriteHeader(int)             {}
func (discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

type responseLimitTestConfig struct {
	sdk.PluginConfig
	mib int
}

func (c responseLimitTestConfig) GetInt(string) int { return c.mib }

func TestStreamTrafficLimitConfiguration(t *testing.T) {
	for _, test := range []struct{ mib, want int }{{0, 384}, {-1, 384}, {111, 384}, {112, 112}, {512, 512}, {4096, 4096}, {4097, 384}} {
		g := &OpenAIGateway{config: responseLimitTestConfig{mib: test.mib}}
		if got := g.streamResponseLimitBytes(); got != test.want<<20 {
			t.Fatalf("config %d: got %d, want %d MiB", test.mib, got, test.want)
		}
	}
}
