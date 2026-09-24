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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/DevilGenius/airgate-sdk/protocol/proto"
	sdkgrpc "github.com/DevilGenius/airgate-sdk/runtimego/grpc"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type imageOwnerWriter struct {
	*httptest.ResponseRecorder
	ping                  func()
	writeErr              error
	marked                bool
	terminalWithoutMarker bool
}

func (w *imageOwnerWriter) BeginStreamCompletion() { w.marked = true }
func (w *imageOwnerWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	n, err := w.ResponseRecorder.Write(p)
	if strings.Contains(string(p), "event: ping") && w.ping != nil {
		w.ping()
	}
	if strings.Contains(string(p), "image_generation.completed") && !w.marked {
		w.terminalWithoutMarker = true
	}
	return n, err
}

func imageOwnerRequest(w http.ResponseWriter) *sdk.ForwardRequest {
	return &sdk.ForwardRequest{Stream: true, Writer: w, Body: []byte(`{"model":"gpt-image-2","stream":true}`), Headers: http.Header{"X-Forwarded-Path": {"/v1/images/generations"}, "Content-Type": {"application/json"}}, Account: &sdk.Account{Credentials: map[string]string{"api_key": "test"}}}
}

func imageOwnerSuccess() sdk.ForwardOutcome {
	return sdk.ForwardOutcome{Kind: sdk.OutcomeSuccess, Usage: &sdk.Usage{Model: "gpt-image-2", OutputTokens: 7, OutputCost: 0.2}, Upstream: sdk.UpstreamResponse{StatusCode: 200, Body: []byte(`{"data":[{"b64_json":"image"}]}`)}}
}

func TestImageResponseOwnsPingAndCompletion(t *testing.T) {
	started, finish, pinged := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ticks := make(chan time.Time)
	w := &imageOwnerWriter{ResponseRecorder: httptest.NewRecorder(), ping: func() { close(pinged) }}
	req := imageOwnerRequest(w)
	sharedMutation := false
	w.ping = func() {
		sharedMutation = req.Headers.Get("X-Worker") != "" || req.Account.Credentials["api_key"] != "test" || req.Body[0] != '{'
		close(pinged)
	}
	go func() { <-started; ticks <- time.Now(); <-pinged; close(finish) }()
	out, err := runImageResponse(t.Context(), req, func(_ context.Context, work *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
		if work.Writer != nil {
			return sdk.ForwardOutcome{}, errors.New("worker received Writer")
		}
		// Mutations must not race with parent-side finalization after cancellation.
		work.Headers.Set("X-Worker", "changed")
		work.Account.Credentials["api_key"] = "changed"
		work.Body[0] = ' '
		close(started)
		<-finish
		return imageOwnerSuccess(), nil
	}, ticks)
	if err != nil || out.Kind != sdk.OutcomeSuccess || out.Usage.OutputTokens != 7 || len(out.Upstream.Body) != 0 {
		t.Fatalf("outcome=%+v err=%v", out, err)
	}
	if !strings.HasPrefix(w.Body.String(), "event: ping\ndata: {}\n\n") || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") || w.terminalWithoutMarker {
		t.Fatal("ping/terminal order changed")
	}
	if sharedMutation {
		t.Fatal("worker shared mutable request state")
	}
	if req.Writer != w || req.Headers.Get("X-Worker") != "changed" {
		t.Fatal("completed request normalization was not transferred back to its owner")
	}
}

func TestImageResponseFailureBeforePingDoesNotCommit(t *testing.T) {
	w := &imageOwnerWriter{ResponseRecorder: httptest.NewRecorder()}
	out, err := runImageResponse(t.Context(), imageOwnerRequest(w), func(context.Context, *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
		return sdk.ForwardOutcome{Kind: sdk.OutcomeAccountRateLimited, Upstream: sdk.UpstreamResponse{StatusCode: 429, Body: []byte(`{"error":"limit"}`)}}, nil
	}, nil)
	if err != nil || out.Kind != sdk.OutcomeAccountRateLimited || out.Upstream.StatusCode != 429 || out.FailoverScope == sdk.FailoverScopeTerminal || w.Body.Len() != 0 || w.marked || len(w.Header()) != 0 {
		t.Fatalf("uncommitted failure lost retry/HTTP semantics: %+v %v", out, err)
	}
}

func TestImageResponseFailureAfterPingIsSanitizedAndTerminal(t *testing.T) {
	for _, safety := range []bool{false, true} {
		started, finish, pinged := make(chan struct{}), make(chan struct{}), make(chan struct{})
		ticks := make(chan time.Time)
		w := &imageOwnerWriter{ResponseRecorder: httptest.NewRecorder(), ping: func() { close(pinged) }}
		go func() { <-started; ticks <- time.Now(); <-pinged; close(finish) }()
		out, _ := runImageResponse(t.Context(), imageOwnerRequest(w), func(context.Context, *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
			close(started)
			<-finish
			if safety {
				return imageSafetyClientOutcome("private upstream detail"), nil
			}
			return transientOutcome("private upstream detail"), errors.New("private upstream detail")
		}, ticks)
		if out.FailoverScope != sdk.FailoverScopeTerminal || !w.marked || strings.Contains(w.Body.String(), "private upstream detail") || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
			t.Fatal("committed failure escaped terminal/error policy")
		}
		if safety && !strings.Contains(w.Body.String(), imageSafetyInvalidRequestCode) {
			t.Fatal("normalized safety code was lost")
		}
	}
}

func TestImageResponseCancellationAllowsLateResultWithoutLateWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	w := &imageOwnerWriter{ResponseRecorder: httptest.NewRecorder()}
	go func() { <-started; cancel() }()
	out, err := runImageResponse(ctx, imageOwnerRequest(w), func(ctx context.Context, work *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
		if work.Writer != nil {
			return sdk.ForwardOutcome{}, errors.New("worker received Writer")
		}
		close(started)
		<-ctx.Done()
		<-release // model a slow upstream cleanup after the caller has returned
		defer close(returned)
		return imageOwnerSuccess(), nil
	}, nil)
	if !errors.Is(err, context.Canceled) || out.Kind != sdk.OutcomeStreamAborted {
		t.Fatalf("cancellation=%+v %v", out, err)
	}
	close(release)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("worker stranded on cancellation")
	}
	if w.Body.Len() != 0 {
		t.Fatal("late result wrote to downstream")
	}
}

func TestImageResponsePingWriteErrorCancelsGeneration(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	ticks := make(chan time.Time)
	w := &imageOwnerWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: io.ErrClosedPipe}
	go func() { <-started; ticks <- time.Now() }()
	out, err := runImageResponse(t.Context(), imageOwnerRequest(w), func(ctx context.Context, _ *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return sdk.ForwardOutcome{}, ctx.Err()
	}, ticks)
	if !errors.Is(err, io.ErrClosedPipe) || out.Kind != sdk.OutcomeStreamAborted {
		t.Fatalf("write failure=%+v %v", out, err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream was not canceled")
	}
}

func TestImageResponseFinalWriteErrorRetainsUsage(t *testing.T) {
	w := &imageOwnerWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: io.ErrClosedPipe}
	out, err := runImageResponse(t.Context(), imageOwnerRequest(w), func(context.Context, *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
		return imageOwnerSuccess(), nil
	}, nil)
	if !errors.Is(err, io.ErrClosedPipe) || out.Kind != sdk.OutcomeStreamAborted || out.Usage == nil || out.Usage.OutputTokens != 7 || out.Usage.OutputCost != 0.2 {
		t.Fatalf("confirmed usage lost: %+v %v", out, err)
	}
}

func TestImageResponseOversizedFinalBodyKeepsUsage(t *testing.T) {
	w := &imageOwnerWriter{ResponseRecorder: httptest.NewRecorder()}
	out, err := runImageResponse(t.Context(), imageOwnerRequest(w), func(context.Context, *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
		out := imageOwnerSuccess()
		out.Upstream.Body = make([]byte, sdk.MaxBufferedResponseBytes+1)
		return out, nil
	}, nil)
	if !errors.Is(err, errResponseTooLarge) || out.Upstream.StatusCode != 502 || out.FailoverScope != sdk.FailoverScopeTerminal || out.Usage == nil || w.Body.Len() != 0 {
		t.Fatalf("oversized final result=%+v %v", out, err)
	}
}

func TestImageResponseRelaysWorkerPanicToRequestOwner(t *testing.T) {
	defer func() {
		if recover() != "image worker failed" {
			t.Fatal("worker panic did not reach the request owner")
		}
	}()
	_, _ = runImageResponse(t.Context(), imageOwnerRequest(httptest.NewRecorder()), func(context.Context, *sdk.ForwardRequest) (sdk.ForwardOutcome, error) { panic("image worker failed") }, nil)
}

type imageCompletionCancelWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *imageCompletionCancelWriter) Flush() {
	if strings.Contains(w.Body.String(), "image_generation.completed") && strings.HasSuffix(w.Body.String(), "\n\n") {
		w.cancel()
	}
}

func TestImageResponseAcrossGRPCSurvivesCompletionCancel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"gpt-image-2","size":"1024x1024","data":[{"b64_json":"image"}],"usage":{"input_tokens":10}}`)
	}))
	t.Cleanup(upstream.Close)
	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), transportPool: NewTransportPool()}
	t.Cleanup(g.transportPool.CloseIdle)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterGatewayServiceServer(server, &sdkgrpc.GatewayGRPCServer{Impl: g})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///image-owner", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
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
	w := &imageCompletionCancelWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	req := imageOwnerRequest(w)
	req.Headers.Set("X-Airgate-Operation-Images-Generate", "true")
	req.Account = &sdk.Account{ID: 9877, Type: "apikey", Platform: "openai", Credentials: map[string]string{"api_key": "test", "base_url": upstream.URL}}
	req.Model = "gpt-image-2"
	req.Body = []byte(`{"model":"gpt-image-2","prompt":"hello","stream":true}`)
	out, err := client.(sdk.GatewayPlugin).Forward(ctx, req)
	if err != nil || out.Kind != sdk.OutcomeSuccess || out.Usage == nil || out.Usage.Model != "gpt-image-2" || out.Usage.OutputCost <= 0 {
		t.Fatalf("image completion lost usage: %+v %v", out, err)
	}
	if ctx.Err() != context.Canceled || !strings.Contains(w.Body.String(), "image_generation.completed") {
		t.Fatal("fixture did not cancel on terminal image output")
	}
}
