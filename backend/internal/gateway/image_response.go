package gateway

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type imageGenerator func(context.Context, *sdk.ForwardRequest) (sdk.ForwardOutcome, error)

type imageResult struct {
	outcome    sdk.ForwardOutcome
	err        error
	panicValue any
	request    *sdk.ForwardRequest
}

// forwardImageResponse is the sole owner of the downstream Writer. Generators
// receive an isolated request with no Writer and return a buffered REST result.
// Text streams never enter this controller or pay for an extra channel/goroutine.
func forwardImageResponse(ctx context.Context, req *sdk.ForwardRequest, generate imageGenerator) (sdk.ForwardOutcome, error) {
	if !req.Stream || req.Writer == nil {
		work := *req
		work.Writer = nil
		outcome, err := generate(ctx, &work)
		applyGeneratedImageRequest(req, &work)
		return outcome, err
	}
	ticker := time.NewTicker(imageKeepAliveInterval)
	defer ticker.Stop()
	return runImageResponse(ctx, req, generate, ticker.C)
}

func runImageResponse(ctx context.Context, req *sdk.ForwardRequest, generate imageGenerator, ticks <-chan time.Time) (sdk.ForwardOutcome, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := *req
	work.Writer = nil
	work.Body = bytes.Clone(req.Body)
	work.Headers = req.Headers.Clone()
	if req.Account != nil {
		account := *req.Account
		account.Credentials = maps.Clone(req.Account.Credentials)
		work.Account = &account
	}
	// A canceled caller does not strand the worker on its final send. The worker
	// must obey ctx, and cannot touch the downstream even if it returns late.
	results := make(chan imageResult, 1)
	go func() {
		result := imageResult{request: &work}
		defer func() {
			result.panicValue = recover()
			results <- result
		}()
		result.outcome, result.err = generate(ctx, &work)
	}()
	started := false
	for {
		// Prefer an already available result to another ping.
		select {
		case result := <-results:
			return finishImageResponse(ctx, req, result, started)
		default:
		}
		select {
		case result := <-results:
			return finishImageResponse(ctx, req, result, started)
		case <-ticks:
			if ctx.Err() != nil {
				continue
			}
			prepareImageSSEHeaders(req.Writer)
			if err := writeSSEPing(req.Writer); err != nil {
				return abortedImageResponse(sdk.ForwardOutcome{}, err)
			}
			started = true
		case <-ctx.Done():
			select {
			case result := <-results:
				return finishImageResponse(ctx, req, result, started)
			default:
				return abortedImageResponse(sdk.ForwardOutcome{}, context.Cause(ctx))
			}
		}
	}
}

func prepareImageSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
}

func finishImageResponse(ctx context.Context, req *sdk.ForwardRequest, result imageResult, started bool) (sdk.ForwardOutcome, error) {
	if result.panicValue != nil {
		panic(result.panicValue)
	}
	applyGeneratedImageRequest(req, result.request)
	outcome, err := result.outcome, result.err
	if errors.Is(err, errResponseTooLarge) || len(outcome.Upstream.Body) > sdk.MaxBufferedResponseBytes {
		responseLimitFor(ctx).trip()
		err = errResponseTooLarge
		outcome.Kind = sdk.OutcomeUpstreamTransient
		outcome.FailoverScope = sdk.FailoverScopeTerminal
		outcome.Reason = err.Error()
		outcome.Upstream = sdk.UpstreamResponse{StatusCode: http.StatusBadGateway, Headers: http.Header{"Content-Type": {"application/json"}}, Body: openAIErrorJSON("server_error", "upstream_response_too_large", err.Error())}
	} else if ctx.Err() != nil {
		return abortedImageResponse(outcome, context.Cause(ctx))
	}
	if err != nil || outcome.Kind != sdk.OutcomeSuccess {
		if started {
			outcome.FailoverScope = sdk.FailoverScopeTerminal
			if writeErr := writeImageOutcomeErrorSSE(req.Writer, outcome); writeErr != nil {
				return abortedImageResponse(outcome, writeErr)
			}
		}
		return outcome, err
	}
	prepareImageSSEHeaders(req.Writer)
	_, path := resolveAPIKeyRoute(req)
	if writeErr := writeImagesRESTSSE(req.Writer, outcome.Upstream.Body, isImagesEditRequest(path)); writeErr != nil {
		return abortedImageResponse(outcome, writeErr)
	}
	outcome.Upstream.Body = nil
	outcome.Upstream.Headers = http.Header{"Content-Type": {"text/event-stream"}}
	return outcome, nil
}

// Transfer normalized request fields back only after the worker has completed.
// This preserves model rerouting/body normalization without sharing mutations
// with a canceled request's finalization. The Writer always stays with its owner.
func applyGeneratedImageRequest(req, generated *sdk.ForwardRequest) {
	if generated == nil {
		return
	}
	w := req.Writer
	*req = *generated
	req.Writer = w
}

func abortedImageResponse(outcome sdk.ForwardOutcome, err error) (sdk.ForwardOutcome, error) {
	outcome.Kind = sdk.OutcomeStreamAborted
	outcome.FailoverScope = sdk.FailoverScopeTerminal
	outcome.Reason = err.Error()
	outcome.Upstream = sdk.UpstreamResponse{StatusCode: http.StatusOK}
	return outcome, err
}
