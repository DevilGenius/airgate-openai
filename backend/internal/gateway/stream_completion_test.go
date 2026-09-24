package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/DevilGenius/airgate-sdk/protocol/proto"
	sdkgrpc "github.com/DevilGenius/airgate-sdk/runtimego/grpc"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// A server can keep its HTTP connection open after the terminal event. Reading
// again would yield cancellation, exactly as in the production failure report.
type completionBody struct {
	*strings.Reader
	readsAfterData int
	closed         bool
}

func (b *completionBody) Read(p []byte) (int, error) {
	if b.Reader.Len() == 0 {
		b.readsAfterData++
		return 0, context.Canceled
	}
	return b.Reader.Read(p)
}
func (b *completionBody) Close() error { b.closed = true; return nil }

type completionRecorder struct {
	*httptest.ResponseRecorder
	marked               bool
	markedBeforeTerminal bool
}

func (w *completionRecorder) BeginStreamCompletion() { w.marked = true }
func (w *completionRecorder) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "response.completed") || strings.Contains(string(p), "response.done") || strings.Contains(string(p), "message_stop") || strings.Contains(string(p), "[DONE]") {
		w.markedBeforeTerminal = w.marked
	}
	return w.ResponseRecorder.Write(p)
}

func TestCompletedSSEStopsWithoutWaitingForEOF(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.done", "[DONE]"} {
		t.Run(terminal, func(t *testing.T) {
			data := `data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n"
			if terminal == "[DONE]" {
				data += `data: {"model":"gpt-6-astra","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7}}` + "\n\ndata: [DONE]\n"
			} else {
				data += `event: ` + terminal + "\n" + `data: {"type":"` + terminal + `","response":{"id":"resp_completed","status":"completed","model":"gpt-6-astra","usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":20}}}}` + "\n"
			}
			body := &completionBody{Reader: strings.NewReader(data)}
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}
			w := &completionRecorder{ResponseRecorder: httptest.NewRecorder()}
			outcome, err := handleStreamResponse(resp, &limitedResponseWriter{ResponseWriter: w, stream: true}, time.Now(), "")
			if err != nil || outcome.Kind != sdk.OutcomeSuccess || outcome.Usage == nil || outcome.Usage.OutputTokens != 7 {
				t.Fatalf("completion lost usage: kind=%v err=%v", outcome.Kind, err)
			}
			if body.readsAfterData != 0 || !body.closed || !w.markedBeforeTerminal {
				t.Fatal("completion still waits for EOF or bypasses SDK ordering")
			}
			if !strings.HasSuffix(w.Body.String(), "\n\n") {
				t.Fatal("terminal SSE delimiter was not written")
			}
			if terminal != "[DONE]" && (outcome.Usage.InputTokens != 80 || outcome.Usage.CachedInputTokens != 20) {
				t.Fatal("cached token split changed")
			}
		})
	}
}

func TestSSECancelBeforeCompletionIsNotSuccess(t *testing.T) {
	body := &completionBody{Reader: strings.NewReader(`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n")}
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}
	outcome, err := handleStreamResponse(resp, httptest.NewRecorder(), time.Now(), "")
	if outcome.Kind != sdk.OutcomeStreamAborted || err == nil || outcome.Usage != nil {
		t.Fatalf("partial output became success or invented usage: kind=%v err=%v", outcome.Kind, err)
	}
}

func TestMalformedSSECompletionIsNotSuccess(t *testing.T) {
	body := `data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed"` + "\n"
	outcome, err := handleStreamResponse(&http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, httptest.NewRecorder(), time.Now(), "")
	if err == nil || outcome.Kind == sdk.OutcomeSuccess {
		t.Fatal("malformed completion accepted")
	}
}

func TestAnthropicCompletionStopsWithoutWaitingForEOF(t *testing.T) {
	body := &completionBody{Reader: strings.NewReader(`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n" + `data: {"type":"response.completed","response":{"id":"resp_done","status":"completed","model":"gpt-6-astra","usage":{"input_tokens":10,"output_tokens":7}}}` + "\n")}
	w := &completionRecorder{ResponseRecorder: httptest.NewRecorder()}
	outcome, err := translateResponsesSSEToAnthropicSSE(t.Context(), &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, w, "claude", "gpt-6-astra", nil, "", "", time.Now(), 0, openAISessionResolution{})
	if err != nil || outcome.Kind != sdk.OutcomeSuccess || outcome.Usage == nil || outcome.Usage.OutputTokens != 7 || !w.markedBeforeTerminal || body.readsAfterData != 0 || !body.closed {
		t.Fatalf("Anthropic completion failed: kind=%v err=%v", outcome.Kind, err)
	}
}

type failedCompletionWriter struct{ *httptest.ResponseRecorder }

func (w failedCompletionWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCompletedSSEWriteFailureKeepsUsage(t *testing.T) {
	body := `data: {"type":"response.completed","response":{"id":"resp_done","status":"completed","model":"gpt-6-astra","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":7}}}` + "\n"
	outcome, err := handleStreamResponse(&http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, failedCompletionWriter{httptest.NewRecorder()}, time.Now(), "")
	if !errors.Is(err, io.ErrClosedPipe) || outcome.Kind != sdk.OutcomeStreamAborted || outcome.Usage == nil || outcome.Usage.OutputTokens != 7 {
		t.Fatalf("write failure lost usage: kind=%v err=%v", outcome.Kind, err)
	}
}

type closeAfterCompletionWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *closeAfterCompletionWriter) Flush() {
	if strings.Contains(w.Body.String(), `"type":"response.completed"`) && strings.HasSuffix(w.Body.String(), "\n\n") {
		w.cancel()
	}
}

func TestOpenAICompletionAcrossGRPCPreservesUsage(t *testing.T) {
	upstreamClosed := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"hello"}`+"\n\n"+
			`event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"id":"resp_integration","status":"completed","model":"gpt-6-astra","usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":20}}}}`+"\n\n")
		w.(http.Flusher).Flush()
		// Keep the response open. A correct plugin closes it at the terminal event.
		<-r.Context().Done()
		upstreamClosed <- struct{}{}
	}))
	t.Cleanup(upstream.Close)
	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), transportPool: NewTransportPool()}
	t.Cleanup(g.transportPool.CloseIdle)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterGatewayServiceServer(server, &sdkgrpc.GatewayGRPCServer{Impl: g})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///openai-completion", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	adapter := &sdkgrpc.GatewayGRPCPlugin{}
	client, err := adapter.GRPCClient(t.Context(), nil, conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	w := &closeAfterCompletionWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	outcome, err := client.(sdk.GatewayPlugin).Forward(ctx, &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 9876, Type: "apikey", Platform: "openai", Credentials: map[string]string{"api_key": "test", "base_url": upstream.URL}},
		Model:   "gpt-6-astra", Stream: true, Writer: w, Body: []byte(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`),
		Headers: http.Header{"X-Forwarded-Path": {"/v1/responses"}, "X-Forwarded-Method": {"POST"}},
	})
	if err != nil || outcome.Kind != sdk.OutcomeSuccess || outcome.Usage == nil || outcome.Usage.OutputTokens != 7 || outcome.Usage.InputTokens != 80 || outcome.Usage.CachedInputTokens != 20 {
		t.Fatalf("completed response lost accounting across gRPC: kind=%v usage=%+v err=%v", outcome.Kind, outcome.Usage, err)
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("downstream did not cancel on receipt of completion")
	}
	select {
	case <-upstreamClosed:
	case <-time.After(time.Second):
		t.Fatal("upstream not closed after completion")
	}
}

func TestIncompleteClassificationMatchesAcrossTransports(t *testing.T) {
	for _, reason := range []string{"max_output_tokens", "content_filter"} {
		t.Run(reason, func(t *testing.T) {
			events := []string{
				`{"type":"response.output_text.delta","delta":"hello"}`,
				`{"type":"response.incomplete","response":{"id":"resp_limit","model":"gpt-6-astra","status":"incomplete","incomplete_details":{"reason":"` + reason + `"},"usage":{"input_tokens":10,"output_tokens":7}}}`,
			}
			wantSuccess := reason == "max_output_tokens"
			body := &completionBody{Reader: strings.NewReader("data: " + strings.Join(events, "\n\ndata: ") + "\n\n")}
			out, err := handleStreamResponse(&http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, httptest.NewRecorder(), time.Now(), "")
			if (out.Kind == sdk.OutcomeSuccess) != wantSuccess || (err == nil) != wantSuccess || out.Usage == nil || out.Usage.OutputTokens != 7 {
				t.Fatalf("passthrough classification: %+v %v", out, err)
			}
			for _, transport := range []string{"sse", "ws"} {
				result := parseOutputEventsForTest(t, transport, events, nil)
				if (result.Err == nil) != wantSuccess || result.OutputTokens != 7 {
					t.Fatalf("%s disagrees: err=%v tokens=%d", transport, result.Err, result.OutputTokens)
				}
			}
		})
	}
}
