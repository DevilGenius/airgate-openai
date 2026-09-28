package basispoints

import (
	"bytes"
	"context"
	"io"
)

// PreparedRequest owns only one request's wire body and response translation.
// It retains no state across requests and must be consumed by one stream.
// Full client history is authoritative, including after a restart or route change.
type PreparedRequest struct {
	body   []byte
	bridge *Bridge
	images map[string]InlineImage
}

// PrepareRequest is side-effect free. Identity is only a seed for upstream
// task/turn/cache identifiers, not a key into local conversation state.
// Prepare owns all BAS compatibility conversion and wire-field filtering.
// Invalid requests fail explicitly; selecting BAS never falls back to native OAuth.
func PrepareRequest(body []byte, identity string) (*PreparedRequest, error) {
	body, images, err := planInlineImages(body)
	if err != nil {
		return nil, err
	}
	prepared, bridge, err := Prepare(body, identity, nil)
	if err != nil {
		return nil, err
	}
	return &PreparedRequest{body: prepared, bridge: bridge, images: images}, nil
}

// Body returns a fresh reader; the prepared wire body cannot be mutated by callers.
func (r *PreparedRequest) Body() io.Reader {
	if len(r.images) != 0 {
		return pendingImageReader{}
	}
	return bytes.NewReader(r.body)
}

// Stream takes ownership of upstream. Closing the returned body cancels protocol
// translation and closes upstream, including a read or pipe write in progress.
func (r *PreparedRequest) Stream(ctx context.Context, upstream io.ReadCloser) io.ReadCloser {
	return r.bridge.stream(ctx, upstream, nil, nil)
}
