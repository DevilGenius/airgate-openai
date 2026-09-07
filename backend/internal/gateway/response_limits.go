package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"github.com/tidwall/gjson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errResponseTooLarge = errors.New("上游响应超过大小限制")

type responseLimitKey struct{}
type responseLimit struct {
	exceeded atomic.Bool
	cancel   context.CancelCauseFunc
}

func (l *responseLimit) trip() {
	if l != nil {
		l.exceeded.Store(true)
		l.cancel(errResponseTooLarge)
	}
}
func responseLimitFor(ctx context.Context) *responseLimit {
	if ctx == nil {
		return nil
	}
	limit, _ := ctx.Value(responseLimitKey{}).(*responseLimit)
	return limit
}

func responseRequestContext(resp *http.Response) context.Context {
	if resp.Request != nil {
		return resp.Request.Context()
	}
	return context.Background()
}

func (g *OpenAIGateway) Forward(ctx context.Context, req *sdk.ForwardRequest) (sdk.ForwardOutcome, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	limit := &responseLimit{cancel: cancel}
	ctx = context.WithValue(ctx, responseLimitKey{}, limit)
	copy := *req
	var writer *limitedResponseWriter
	if req.Writer != nil {
		writer = &limitedResponseWriter{ResponseWriter: req.Writer, limit: limit}
		copy.Writer = writer
	}
	outcome, err := g.forwardWithinResponseLimit(ctx, &copy)
	if errors.Is(err, errResponseTooLarge) || len(outcome.Upstream.Body) > sdk.MaxBufferedResponseBytes {
		limit.trip()
	}
	if limit.exceeded.Load() {
		usage, elapsed := outcome.Usage, outcome.Duration
		outcome = sdk.ForwardOutcome{Kind: sdk.OutcomeUpstreamTransient, FailoverScope: sdk.FailoverScopeTerminal, Reason: errResponseTooLarge.Error(), Usage: usage, Duration: elapsed,
			Upstream: sdk.UpstreamResponse{StatusCode: http.StatusBadGateway, Headers: http.Header{"Content-Type": {"application/json"}, "X-Should-Retry": {"false"}}, Body: openAIErrorJSON("server_error", "upstream_response_too_large", errResponseTooLarge.Error())}}
		if req.Stream && writer != nil && writer.wrote.Load() {
			outcome.Kind = sdk.OutcomeStreamAborted
			outcome.Upstream.Body = nil
		}
		return outcome, errResponseTooLarge
	}
	return outcome, err
}

type limitedResponseWriter struct {
	http.ResponseWriter
	limit *responseLimit
	bytes atomic.Int64
	wrote atomic.Bool
}

func (w *limitedResponseWriter) Write(p []byte) (int, error) {
	if w.bytes.Add(int64(len(p))) > sdk.MaxBufferedResponseBytes {
		w.limit.trip()
		return 0, errResponseTooLarge
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.wrote.Store(true)
	}
	if status.Code(err) == codes.ResourceExhausted {
		w.limit.trip()
	}
	return n, err
}
func (w *limitedResponseWriter) WriteHeader(code int) {
	if code >= 200 {
		w.wrote.Store(true)
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *limitedResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func readResponseBody(resp *http.Response) ([]byte, error) {
	var limit *responseLimit
	if resp.Request != nil {
		limit = responseLimitFor(resp.Request.Context())
	}
	if resp.ContentLength > sdk.MaxBufferedResponseBytes {
		limit.trip()
		_ = resp.Body.Close()
		return nil, errResponseTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, sdk.MaxBufferedResponseBytes+1))
	if len(data) > sdk.MaxBufferedResponseBytes {
		limit.trip()
		_ = resp.Body.Close()
		return nil, errResponseTooLarge
	}
	return data, err
}

type failedResponseReader struct{ err error }

func (r failedResponseReader) Read([]byte) (int, error) { return 0, r.err }

type streamResponseBudget struct {
	total, text, reasoning, control, tools, images int
	limit                                          *responseLimit
}

func (b *streamResponseBudget) take(event string, data []byte, wireBytes ...int) error {
	n := len(data)
	if len(wireBytes) > 0 {
		n = wireBytes[0]
	}
	b.total += max(1, n)
	switch {
	case strings.Contains(event, "output_text"):
		b.text += n
	case strings.Contains(event, "reasoning"):
		b.reasoning += n
	case strings.Contains(event, "image_generation") || gjson.GetBytes(data, "item.type").String() == "image_generation_call":
		b.images += n
	case strings.Contains(event, "function_call") || gjson.GetBytes(data, "item.type").String() == "function_call":
		b.tools += n
	case event == "" || event == "response.created" || event == "response.in_progress" || event == "codex.rate_limits":
		b.control += n
	}
	if b.total > sdk.MaxBufferedResponseBytes || b.text > 8<<20 || b.reasoning > 8<<20 || b.control > 1<<20 || b.tools > 8<<20 || b.images > 24<<20 {
		b.limit.trip()
		return errResponseTooLarge
	}
	return nil
}

func handlerResponseError(handler WSEventHandler) error {
	if h, ok := handler.(interface{ Err() error }); ok {
		return h.Err()
	}
	return nil
}
