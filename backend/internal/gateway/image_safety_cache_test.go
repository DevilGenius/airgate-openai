package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestImageSafetyRequestCacheTTLAndCapacity(t *testing.T) {
	cache := imageSafetyRequestCache{
		ttl:        10 * time.Minute,
		maxEntries: 2,
	}
	start := time.Unix(1_700_000_000, 0)
	const (
		first  = uint64(1)
		second = uint64(2)
		third  = uint64(3)
	)

	cache.add(first, start)
	cache.add(second, start.Add(time.Minute))
	if !cache.contains(first, start.Add(2*time.Minute)) || !cache.contains(second, start.Add(2*time.Minute)) {
		t.Fatal("fresh safety request hashes should be cached")
	}

	cache.add(third, start.Add(2*time.Minute))
	if cache.contains(first, start.Add(3*time.Minute)) {
		t.Fatal("oldest entry should be evicted at capacity")
	}
	if !cache.contains(second, start.Add(3*time.Minute)) || !cache.contains(third, start.Add(3*time.Minute)) {
		t.Fatal("newest entries should remain after bounded eviction")
	}
	if cache.contains(second, start.Add(11*time.Minute)) {
		t.Fatal("entry should expire after its TTL")
	}
}

func TestImageSafetyRequestHashSeparatesDifferentRequests(t *testing.T) {
	base := &sdk.ForwardRequest{
		Body:    []byte(`{"model":"gpt-image-2","prompt":"same prompt"}`),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Model:   "gpt-image-2",
	}
	first, ok := imageSafetyRequestHash(base, http.MethodPost, "/v1/images/generations")
	if !ok {
		t.Fatal("image request should produce a hash")
	}
	copyReq := *base
	second, ok := imageSafetyRequestHash(&copyReq, http.MethodPost, "/v1/images/generations")
	if !ok || first != second {
		t.Fatal("identical image requests should produce the same hash")
	}
	changedReq := *base
	changedReq.Body = []byte(`{"model":"gpt-image-2","prompt":"different prompt"}`)
	changed, _ := imageSafetyRequestHash(&changedReq, http.MethodPost, "/v1/images/generations")
	if first == changed {
		t.Fatal("different image requests must not share a hash")
	}
	if _, ok := imageSafetyRequestHash(base, http.MethodPost, "/v1/responses"); ok {
		t.Fatal("non-image request should not produce an image safety hash")
	}
}

func TestImageSafetyRequestCacheRejectsDuplicateWithoutAccountFailure(t *testing.T) {
	gateway := &OpenAIGateway{}
	req := &sdk.ForwardRequest{
		Body:    []byte(`{"model":"gpt-image-2","prompt":"blocked prompt"}`),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Model:   "gpt-image-2",
	}
	ctx, outcome := gateway.checkImageSafetyRequest(context.Background(), req, http.MethodPost, "/v1/images/generations")
	if outcome != nil {
		t.Fatal("uncached request should continue to upstream")
	}
	gateway.rememberImageSafetyRequest(ctx)
	_, outcome = gateway.checkImageSafetyRequest(context.Background(), req, http.MethodPost, "/v1/images/generations")
	if outcome == nil {
		t.Fatal("cached request should be rejected locally")
	}
	if outcome.Kind != sdk.OutcomeClientError || outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Fatalf("outcome = %#v, want client error 400", outcome)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error", got)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "error.code").String(); got != imageSafetyInvalidRequestCode {
		t.Fatalf("error.code = %q, want %q", got, imageSafetyInvalidRequestCode)
	}
	if err := forwardErrForOutcome(*outcome, errors.New("upstream error")); err != nil {
		t.Fatalf("client error must not trigger account degradation: %v", err)
	}
}

func TestImageTaskInputCarriesOriginalSafetyRequestHash(t *testing.T) {
	req := &sdk.ForwardRequest{
		Body:    []byte(`{"model":"gpt-image-2","prompt":"blocked prompt","size":"1024x1024"}`),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Model:   "gpt-image-2",
	}
	want := imageSafetyRequestHashHex(req, http.MethodPost, "/v1/images/generations")
	input, _, err := imageGenerateHandler{}.BuildInput(req, "/v1/images/generations")
	if err != nil {
		t.Fatalf("BuildInput returned error: %v", err)
	}
	if got, _ := input[imageSafetyRequestHashInputKey].(string); got == "" || got != want {
		t.Fatalf("task request hash = %q, want %q", got, want)
	}
}
