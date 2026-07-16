package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/zeebo/xxh3"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

var imageSafetyRequestHashBenchmarkSink uint64

func TestImageSafetyRequestCacheTTLAndCapacity(t *testing.T) {
	if imageSafetyRequestCacheTTL != 24*time.Hour {
		t.Fatalf("default TTL = %s, want 24h", imageSafetyRequestCacheTTL)
	}
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

func TestImageSafetyRequestHashUsesStableDigest(t *testing.T) {
	req := &sdk.ForwardRequest{
		Body:    []byte(`{"prompt":"same prompt","model":"gpt-image-2"}`),
		Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Model:   "gpt-image-2",
	}
	got, ok := imageSafetyRequestHash(req, http.MethodPost, "/v1/images/generations")
	if !ok {
		t.Fatal("image request should produce a hash")
	}

	var hasher xxh3.Hasher
	for _, part := range [][]byte{
		[]byte(imageSafetyRequestHashDomain),
		[]byte(http.MethodPost),
		[]byte("/v1/images/generations"),
		[]byte("gpt-image-2"),
		[]byte("application/json"),
		[]byte("raw"),
	} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = hasher.Write(size[:])
		_, _ = hasher.Write(part)
	}
	var bodySize [8]byte
	binary.BigEndian.PutUint64(bodySize[:], uint64(len(req.Body)))
	_, _ = hasher.Write(bodySize[:])
	var bodyHash [8]byte
	binary.BigEndian.PutUint64(bodyHash[:], xxh3.Hash(req.Body))
	_, _ = hasher.Write(bodyHash[:])
	want := hasher.Sum64()
	if got != want {
		t.Fatalf("hash = %x, want stable XXH3-64 value %x", got, want)
	}
}

func TestImageSafetyRequestHashIgnoresMultipartBoundary(t *testing.T) {
	buildRequest := func(boundary string) *sdk.ForwardRequest {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.SetBoundary(boundary); err != nil {
			t.Fatalf("SetBoundary(%q): %v", boundary, err)
		}
		if err := writer.WriteField("model", "gpt-image-2"); err != nil {
			t.Fatalf("WriteField(model): %v", err)
		}
		if err := writer.WriteField("prompt", "same prompt"); err != nil {
			t.Fatalf("WriteField(prompt): %v", err)
		}
		part, err := writer.CreateFormFile("image", "input.png")
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := part.Write([]byte("same-image-bytes--image-safety-boundary-one-not-a-delimiter")); err != nil {
			t.Fatalf("write image: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close multipart writer: %v", err)
		}
		return &sdk.ForwardRequest{
			Body:    body.Bytes(),
			Headers: http.Header{"Content-Type": []string{writer.FormDataContentType()}},
			Model:   "gpt-image-2",
		}
	}

	first, ok := imageSafetyRequestHash(buildRequest("image-safety-boundary-one"), http.MethodPost, "/v1/images/edits")
	if !ok {
		t.Fatal("first multipart request should produce a hash")
	}
	second, ok := imageSafetyRequestHash(buildRequest("Image-Safety-Boundary-Two"), http.MethodPost, "/v1/images/edits")
	if !ok {
		t.Fatal("second multipart request should produce a hash")
	}
	if first != second {
		t.Fatalf("equivalent multipart hashes differ: %x != %x", first, second)
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
	const upstreamReason = "upstream safety system rejected the prompt (request_id: req_test)"
	gateway.cacheImageSafetyRejection(ctx, upstreamReason)
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
	if outcome.Reason != upstreamReason {
		t.Fatalf("Reason = %q, want preserved upstream reason %q", outcome.Reason, upstreamReason)
	}
	if outcome.SafetyRejected {
		t.Fatal("local cache hit must not be reported as a new upstream safety rejection")
	}
	if err := forwardErrForOutcome(*outcome, errors.New("upstream error")); err != nil {
		t.Fatalf("client error must not trigger account degradation: %v", err)
	}
}

func TestImageSafetyRequestHashCaptureAllowsForwardLevelCacheWrite(t *testing.T) {
	gateway := &OpenAIGateway{}
	req := &sdk.ForwardRequest{
		Body:    []byte(`{"model":"gpt-image-2","prompt":"blocked prompt"}`),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Model:   "gpt-image-2",
	}
	ctx := withImageSafetyRequestHashCapture(context.Background())
	if _, outcome := gateway.checkImageSafetyRequest(ctx, req, http.MethodPost, "/v1/images/generations"); outcome != nil {
		t.Fatal("uncached request should continue to upstream")
	}

	const upstreamReason = "upstream safety system rejected the prompt"
	upstreamOutcome := imageSafetyClientOutcome(upstreamReason)
	if !upstreamOutcome.SafetyRejected {
		t.Fatal("new image safety rejection must be returned through SafetyRejected")
	}
	gateway.cacheImageSafetyRejection(ctx, upstreamOutcome.Reason)

	_, cachedOutcome := gateway.checkImageSafetyRequest(context.Background(), req, http.MethodPost, "/v1/images/generations")
	if cachedOutcome == nil || cachedOutcome.Reason != upstreamReason {
		t.Fatalf("captured hash cache outcome = %#v, want reason %q", cachedOutcome, upstreamReason)
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

func BenchmarkImageSafetyRequestHash(b *testing.B) {
	for _, testCase := range []struct {
		name string
		size int
	}{
		{name: "1MiB", size: 1 << 20},
		{name: "5MiB", size: 5 << 20},
		{name: "20MiB", size: 20 << 20},
	} {
		b.Run(testCase.name, func(b *testing.B) {
			body := bytes.Repeat([]byte{'a'}, testCase.size)
			req := &sdk.ForwardRequest{
				Body:    body,
				Headers: http.Header{"Content-Type": []string{"application/json"}},
				Model:   "gpt-image-2",
			}
			b.ReportAllocs()
			b.SetBytes(int64(testCase.size))
			b.ResetTimer()
			var hash uint64
			for i := 0; i < b.N; i++ {
				hash, _ = imageSafetyRequestHash(req, http.MethodPost, "/v1/images/generations")
			}
			imageSafetyRequestHashBenchmarkSink = hash
		})
	}
}

func BenchmarkImageSafetyRequestHashMultipart(b *testing.B) {
	for _, testCase := range []struct {
		name string
		size int
	}{
		{name: "1MiB", size: 1 << 20},
		{name: "5MiB", size: 5 << 20},
		{name: "20MiB", size: 20 << 20},
	} {
		b.Run(testCase.name, func(b *testing.B) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			if err := writer.SetBoundary("image-safety-benchmark-boundary"); err != nil {
				b.Fatal(err)
			}
			if err := writer.WriteField("model", "gpt-image-2"); err != nil {
				b.Fatal(err)
			}
			if err := writer.WriteField("prompt", "benchmark prompt"); err != nil {
				b.Fatal(err)
			}
			part, err := writer.CreateFormFile("image", "input.png")
			if err != nil {
				b.Fatal(err)
			}
			if _, err := part.Write(bytes.Repeat([]byte{'a'}, testCase.size)); err != nil {
				b.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				b.Fatal(err)
			}
			req := &sdk.ForwardRequest{
				Body:    body.Bytes(),
				Headers: http.Header{"Content-Type": []string{writer.FormDataContentType()}},
				Model:   "gpt-image-2",
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(req.Body)))
			b.ResetTimer()
			var hash uint64
			for i := 0; i < b.N; i++ {
				hash, _ = imageSafetyRequestHash(req, http.MethodPost, "/v1/images/edits")
			}
			imageSafetyRequestHashBenchmarkSink = hash
		})
	}
}
