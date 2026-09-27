package basispoints

import (
	"bytes"
	"context"
	"io"

	"github.com/tidwall/gjson"
)

// PreparedRequest owns only one request's wire body and response translation.
// It retains no state across requests and must be consumed by one stream.
// Full client history is authoritative, including after a restart or route change.
type PreparedRequest struct {
	body   []byte
	bridge *Bridge
}

// FallbackReason contains fixed diagnostics, never request content.
type FallbackReason string

const (
	FallbackNativeControl    FallbackReason = "native_request_control"
	FallbackNativeAttachment FallbackReason = "native_attachment"
	FallbackServiceTier      FallbackReason = "service_tier"
	FallbackHostedTool       FallbackReason = "hosted_tool"
	FallbackProtocol         FallbackReason = "protocol_incompatible"
)

// PrepareRequest is side-effect free. Identity is only a seed for upstream
// task/turn/cache identifiers, not a key into local conversation state.
// Exactly one of a prepared request or a native-route reason is returned.
func PrepareRequest(body []byte, identity string) (*PreparedRequest, FallbackReason) {
	if reason := requestFallbackReason(body); reason != "" {
		return nil, reason
	}
	prepared, bridge, err := Prepare(body, identity, nil)
	if err != nil {
		return nil, FallbackProtocol
	}
	// Prepare also discovers nested/additional tool declarations. Do not silently
	// omit hosted capabilities hidden inside those declarations.
	if len(bridge.unsupportedTools) != 0 {
		return nil, FallbackHostedTool
	}
	return &PreparedRequest{body: prepared, bridge: bridge}, ""
}

// Body returns a fresh reader; the prepared wire body cannot be mutated by callers.
func (r *PreparedRequest) Body() io.Reader { return bytes.NewReader(r.body) }

// Stream takes ownership of upstream. Closing the returned body cancels protocol
// translation and closes upstream, including a read or pipe write in progress.
// report, when non-nil, receives one content-free summary from the stream goroutine
// after closing the stream. Reading EOF does not wait for the diagnostic callback.
func (r *PreparedRequest) Stream(ctx context.Context, upstream io.ReadCloser, report func(StreamSummary)) io.ReadCloser {
	return r.bridge.stream(ctx, upstream, nil, nil, report)
}

func requestFallbackReason(body []byte) FallbackReason {
	source := gjson.ParseBytes(body)
	// BPS cannot preserve these native controls. Do not silently discard them.
	for _, key := range []string{"previous_response_id", "temperature", "top_p", "max_output_tokens", "max_completion_tokens", "max_tokens", "truncation", "background"} {
		if value := source.Get(key); value.Exists() && value.Type != gjson.Null && value.Raw != "false" && value.String() != "" {
			return FallbackNativeControl
		}
	}
	for _, item := range source.Get("input").Array() {
		for _, field := range []string{"content", "output"} {
			for _, part := range item.Get(field).Array() {
				if part.Get("type").String() == "input_image" && part.Get("file_id").Exists() {
					return FallbackNativeAttachment
				}
			}
		}
	}
	if tier := source.Get("service_tier").String(); tier != "" && tier != "auto" && tier != "default" {
		return FallbackServiceTier
	}
	// Hosted tools must keep their original semantics, including search options.
	for _, tool := range source.Get("tools").Array() {
		switch tool.Get("type").String() {
		case "function", "custom", "namespace":
		default:
			return FallbackHostedTool
		}
	}
	return ""
}
