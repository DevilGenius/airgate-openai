package gateway

import (
	"context"
	"sync/atomic"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// These adapters keep the focused cache tests readable while production code
// only uses the runtime hash lifecycle.

type textRequestHashContextKey struct{}
type textSafetyHashContextKey struct{}

func withTextRequestHash(ctx context.Context, hash uint64) context.Context {
	return context.WithValue(ctx, textRequestHashContextKey{}, hash)
}

func withTextSafetyHash(ctx context.Context, hash uint64) context.Context {
	return context.WithValue(ctx, textSafetyHashContextKey{}, hash)
}

func textRequestHashFromContext(ctx context.Context) (uint64, bool) {
	if ctx == nil {
		return 0, false
	}
	hash, ok := ctx.Value(textRequestHashContextKey{}).(uint64)
	return hash, ok
}

func textSafetyHashFromContext(ctx context.Context) (uint64, bool) {
	if ctx == nil {
		return 0, false
	}
	hash, ok := ctx.Value(textSafetyHashContextKey{}).(uint64)
	return hash, ok
}

func (g *OpenAIGateway) checkTextSafetyRequest(
	ctx context.Context,
	req *sdk.ForwardRequest,
	method, path string,
) (context.Context, *sdk.ForwardOutcome) {
	g.runtimeHash.initialize()
	hash, ok := textRequestHash(req, method, path)
	if !ok {
		return ctx, nil
	}
	safetyHash := textSafetyRequestHash(req, hash)
	if g.runtimeHash.text.textSafety.contains(safetyHash, time.Now()) {
		outcome := textSafetyCacheHitOutcome(isAnthropicTextRequest(req, path))
		return ctx, &outcome
	}
	ctx = withTextRequestHash(ctx, hash)
	return withTextSafetyHash(ctx, safetyHash), nil
}

func (g *OpenAIGateway) cacheTextSafetyRejection(ctx context.Context) {
	g.runtimeHash.initialize()
	if hash, ok := textSafetyHashFromContext(ctx); ok {
		g.runtimeHash.text.textSafety.add(hash, time.Now())
	}
}

func requestRetryCacheForTest(g *OpenAIGateway) *safetyRequestCache {
	g.runtimeHash.initialize()
	return &g.runtimeHash.text.requestRetry
}

func encryptedContentCacheForTest(g *OpenAIGateway) *safetyRequestCache {
	g.runtimeHash.initialize()
	return &g.runtimeHash.text.encryptedContent
}

type encryptedContentRetryRequestState = enabledEncryptedContentHashSession

func (g *OpenAIGateway) newEncryptedContentRetryRequestState() *encryptedContentRetryRequestState {
	g.runtimeHash.initialize()
	return &encryptedContentRetryRequestState{
		cache:     &g.runtimeHash.text.encryptedContent,
		checkedAt: time.Now(),
	}
}

type imageSafetyRequestContextKey struct{}
type imageSafetyRequestHashCaptureContextKey struct{}
type imageSafetyRequestCache = safetyRequestCache

type imageSafetyRequestHashCapture struct {
	hash  atomic.Uint64
	ready atomic.Bool
}

func withImageSafetyRequestHash(ctx context.Context, hash uint64) context.Context {
	return context.WithValue(ctx, imageSafetyRequestContextKey{}, hash)
}

func withImageSafetyRequestHashCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, imageSafetyRequestHashCaptureContextKey{}, &imageSafetyRequestHashCapture{})
}

func captureImageSafetyRequestHash(ctx context.Context, hash uint64) {
	if ctx == nil {
		return
	}
	capture, _ := ctx.Value(imageSafetyRequestHashCaptureContextKey{}).(*imageSafetyRequestHashCapture)
	if capture == nil {
		return
	}
	capture.hash.Store(hash)
	capture.ready.Store(true)
}

func imageSafetyRequestHashFromContext(ctx context.Context) (uint64, bool) {
	if ctx == nil {
		return 0, false
	}
	if hash, ok := ctx.Value(imageSafetyRequestContextKey{}).(uint64); ok {
		return hash, true
	}
	capture, _ := ctx.Value(imageSafetyRequestHashCaptureContextKey{}).(*imageSafetyRequestHashCapture)
	if capture == nil || !capture.ready.Load() {
		return 0, false
	}
	return capture.hash.Load(), true
}

func (g *OpenAIGateway) checkImageSafetyRequest(
	ctx context.Context,
	req *sdk.ForwardRequest,
	method, path string,
) (context.Context, *sdk.ForwardOutcome) {
	g.runtimeHash.initialize()
	hash, ok := imageSafetyRequestHash(req, method, path)
	if !ok {
		return ctx, nil
	}
	captureImageSafetyRequestHash(ctx, hash)
	if reason, cached := g.runtimeHash.image.cache.lookup(hash, time.Now()); cached {
		outcome := imageSafetyClientOutcome(reason)
		outcome.SafetyRejected = false
		return ctx, &outcome
	}
	return withImageSafetyRequestHash(ctx, hash), nil
}

func (g *OpenAIGateway) cacheImageSafetyRejection(ctx context.Context, reasons ...string) {
	g.runtimeHash.initialize()
	reason := ""
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	if hash, ok := imageSafetyRequestHashFromContext(ctx); ok {
		g.runtimeHash.image.cache.addWithReason(hash, time.Now(), reason)
	}
}
