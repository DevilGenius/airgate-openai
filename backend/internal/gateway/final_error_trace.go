package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"github.com/zeebo/xxh3"
)

const (
	maxFinalErrorDiagnosticRequests  = 8
	maxFinalErrorDiagnosticBodyBytes = 48 << 20
)

type finalErrorTraceContextKey struct{}

type finalErrorTraceCapture struct {
	mu                sync.Mutex
	outbound          []*capturedOutboundRequest
	upstreamErrorBody []byte
}

type capturedOutboundRequest struct {
	request    *http.Request
	transport  string
	method     string
	url        string
	headers    http.Header
	body       []byte
	statusCode int
}

type finalErrorTraceRoundTripper struct {
	base    http.RoundTripper
	capture *finalErrorTraceCapture
}

func withFinalErrorTrace(ctx context.Context) (context.Context, *finalErrorTraceCapture) {
	capture := &finalErrorTraceCapture{}
	return context.WithValue(ctx, finalErrorTraceContextKey{}, capture), capture
}

func finalErrorTraceFromContext(ctx context.Context) *finalErrorTraceCapture {
	if ctx == nil {
		return nil
	}
	capture, _ := ctx.Value(finalErrorTraceContextKey{}).(*finalErrorTraceCapture)
	return capture
}

func (g *OpenAIGateway) buildForwardHTTPClient(ctx context.Context, req *sdk.ForwardRequest, account *sdk.Account) *http.Client {
	client := g.buildHTTPClient(account)
	if req == nil || !req.TraceFinalError {
		return client
	}
	capture := finalErrorTraceFromContext(ctx)
	if capture == nil {
		return client
	}
	cloned := *client
	base := cloned.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cloned.Transport = &finalErrorTraceRoundTripper{base: base, capture: capture}
	return &cloned
}

func (t *finalErrorTraceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	entry := &capturedOutboundRequest{request: req, transport: "http"}
	if t.capture != nil {
		t.capture.addOutbound(entry)
	}
	resp, err := base.RoundTrip(req)
	if resp != nil && t.capture != nil {
		t.capture.setStatusCode(entry, resp.StatusCode)
	}
	return resp, err
}

func (c *finalErrorTraceCapture) setStatusCode(entry *capturedOutboundRequest, statusCode int) {
	if c == nil || entry == nil {
		return
	}
	c.mu.Lock()
	entry.statusCode = statusCode
	c.mu.Unlock()
}

func (c *finalErrorTraceCapture) addOutbound(entry *capturedOutboundRequest) {
	if c == nil || entry == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.outbound) < maxFinalErrorDiagnosticRequests {
		c.outbound = append(c.outbound, entry)
		return
	}
	// Keep the first upstream request and the most recent requests. This retains
	// the original submission while bounding polling/retry diagnostics.
	copy(c.outbound[1:maxFinalErrorDiagnosticRequests-1], c.outbound[2:maxFinalErrorDiagnosticRequests])
	c.outbound[maxFinalErrorDiagnosticRequests-1] = entry
}

func captureFinalErrorWebSocketRequest(ctx context.Context, targetURL string, cfg WSConfig, body []byte) {
	capture := finalErrorTraceFromContext(ctx)
	if capture == nil {
		return
	}
	headers := cloneHTTPHeader(cfg.Headers)
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("OpenAI-Beta", WSBetaHeader)
	if cfg.SessionID != "" {
		headers.Set("session_id", cfg.SessionID)
	}
	if cfg.ConversationID != "" {
		headers.Set("conversation_id", cfg.ConversationID)
	}
	if cfg.TurnState != "" {
		headers.Set("x-codex-turn-state", cfg.TurnState)
	}
	if cfg.Originator != "" {
		headers.Set("originator", cfg.Originator)
	}
	capture.addOutbound(&capturedOutboundRequest{
		transport: "websocket",
		method:    "response.create",
		url:       redactURL(targetURL),
		headers:   safeDiagnosticHeaders(headers),
		body:      body,
	})
}

func captureFinalErrorUpstreamBody(ctx context.Context, body []byte) {
	capture := finalErrorTraceFromContext(ctx)
	if capture == nil || len(body) == 0 {
		return
	}
	capture.mu.Lock()
	capture.upstreamErrorBody = body
	capture.mu.Unlock()
}

func (c *finalErrorTraceCapture) snapshot() *sdk.FinalErrorDiagnostic {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	outbound := make([]capturedOutboundRequest, 0, len(c.outbound))
	for _, entry := range c.outbound {
		if entry != nil {
			outbound = append(outbound, *entry)
		}
	}
	upstreamErrorBody := c.upstreamErrorBody
	c.mu.Unlock()

	diagnostic := &sdk.FinalErrorDiagnostic{}
	remainingBodyBytes := maxFinalErrorDiagnosticBodyBytes
	if len(upstreamErrorBody) <= remainingBodyBytes {
		diagnostic.UpstreamErrorBody = upstreamErrorBody
		remainingBodyBytes -= len(upstreamErrorBody)
	}
	if len(outbound) > 0 {
		diagnostic.OutboundRequests = make([]sdk.OutboundRequestDiagnostic, len(outbound))
	}
	// Materialize newest requests first so the actual terminal retry keeps its
	// body when several large internal retries approach the gRPC message limit.
	for index := len(outbound) - 1; index >= 0; index-- {
		entry := outbound[index]
		request := sdk.OutboundRequestDiagnostic{
			Transport:  entry.transport,
			Method:     entry.method,
			URL:        entry.url,
			Headers:    safeDiagnosticHeaders(entry.headers),
			StatusCode: entry.statusCode,
		}
		body := entry.body
		if entry.request != nil {
			request.Method = entry.request.Method
			if entry.request.URL != nil {
				request.URL = redactURL(entry.request.URL.String())
			}
			request.Headers = safeDiagnosticHeaders(entry.request.Header)
			if entry.request.GetBody != nil {
				if reader, err := entry.request.GetBody(); err == nil {
					body, _ = io.ReadAll(reader)
					_ = reader.Close()
				}
			}
		}
		bodySnapshot := sanitizeFinalErrorDiagnosticBody(
			body,
			request.Headers.Get("Content-Type"),
			isImageDiagnosticRequestURL(request.URL),
		)
		request.BodyRedacted = bodySnapshot.Redacted
		request.BodyRedactionReason = bodySnapshot.RedactionReason
		request.BodyOriginalSize = bodySnapshot.OriginalSize
		if bodySnapshot.Redacted && bodySnapshot.ContentType != "" {
			if request.Headers == nil {
				request.Headers = make(http.Header)
			}
			request.Headers.Set("Content-Type", bodySnapshot.ContentType)
		}
		if len(bodySnapshot.Body) <= remainingBodyBytes {
			request.Body = bodySnapshot.Body
			remainingBodyBytes -= len(bodySnapshot.Body)
		}
		diagnostic.OutboundRequests[index] = request
	}
	if len(diagnostic.OutboundRequests) == 0 && len(diagnostic.UpstreamErrorBody) == 0 {
		return nil
	}
	return diagnostic
}

func safeDiagnosticHeaders(headers http.Header) http.Header {
	if len(headers) == 0 {
		return nil
	}
	safe := make(http.Header)
	for name, values := range headers {
		canonical := strings.ToLower(strings.TrimSpace(name))
		switch canonical {
		case "accept", "content-type", "openai-beta", "originator", "user-agent":
			safe[name] = append([]string(nil), values...)
		case "session_id", "conversation_id", "x-codex-turn-state":
			digest := xxh3.HashString(strings.Join(values, "\x00"))
			safe.Set("X-Airgate-Trace-"+strings.ReplaceAll(canonical, "_", "-")+"-XXH3-64", fmt.Sprintf("%016x", digest))
		default:
			if strings.HasPrefix(canonical, "x-airgate-trace-") {
				safe[name] = append([]string(nil), values...)
			}
		}
	}
	if len(safe) == 0 {
		return nil
	}
	return safe
}

func shouldAttachFinalErrorDiagnostic(outcome sdk.ForwardOutcome, err error) bool {
	return err != nil || outcome.Kind != sdk.OutcomeSuccess
}
