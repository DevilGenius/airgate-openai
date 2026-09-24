package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

var errResponseTooLarge = errors.New("上游响应超过大小限制")

const (
	maxResponseEventBytes        = sdk.MaxResponseMessageBytes
	defaultStreamResponseBytes   = 384 << 20
	maxPendingControlBytes       = 1 << 20
	streamResponseLimitConfigKey = "stream_response_limit_mib"
)

type responseLimitKey struct{}
type responseLimit struct {
	exceeded    atomic.Bool
	cancel      context.CancelCauseFunc
	streamBytes int
}

func (l *responseLimit) trip() {
	if l != nil {
		l.exceeded.Store(true)
		if l.cancel != nil {
			l.cancel(errResponseTooLarge)
		}
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
	limit := &responseLimit{cancel: cancel, streamBytes: g.streamResponseLimitBytes()}
	ctx = context.WithValue(ctx, responseLimitKey{}, limit)
	copy := *req
	var writer *limitedResponseWriter
	if req.Writer != nil {
		writer = &limitedResponseWriter{ResponseWriter: req.Writer, limit: limit, stream: req.Stream}
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
	limit  *responseLimit
	stream bool
	bytes  atomic.Int64
	wrote  atomic.Bool
}

func (w *limitedResponseWriter) Write(p []byte) (int, error) {
	maxBytes := sdk.MaxBufferedResponseBytes
	if w.stream {
		maxBytes = w.limit.streamLimitBytes()
	}
	if w.bytes.Add(int64(len(p))) > int64(maxBytes) {
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

func (l *responseLimit) streamLimitBytes() int {
	if l != nil && l.streamBytes > 0 {
		return l.streamBytes
	}
	return defaultStreamResponseBytes
}

func (g *OpenAIGateway) streamResponseLimitBytes() int {
	if config := g.pluginConfig(); config != nil {
		mib := config.GetInt(streamResponseLimitConfigKey)
		// A stream must fit one full event; bound configuration arithmetic as well.
		if mib >= maxResponseEventBytes>>20 && mib <= 4096 {
			return mib << 20
		}
	}
	return defaultStreamResponseBytes
}

func handlerResponseError(handler WSEventHandler) error {
	if h, ok := handler.(interface{ Err() error }); ok {
		return h.Err()
	}
	return nil
}
