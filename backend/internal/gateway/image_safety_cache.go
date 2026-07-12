package gateway

import (
	"context"
	"encoding/binary"
	"hash/maphash"
	"net/http"
	"strconv"
	"sync"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const (
	imageSafetyRequestCacheTTL        = 10 * time.Minute
	imageSafetyRequestCacheMaxEntries = 8192
	imageSafetyRequestHashInputKey    = "_image_safety_request_hash"
)

type imageSafetyRequestContextKey struct{}

var imageSafetyRequestHashSeed = maphash.MakeSeed()

type imageSafetyRequestCache struct {
	mu         sync.Mutex
	entries    map[uint64]time.Time
	ttl        time.Duration
	maxEntries int
}

func (c *imageSafetyRequestCache) contains(hash uint64, now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expiresAt, ok := c.entries[hash]
	if !ok {
		return false
	}
	if !now.Before(expiresAt) {
		delete(c.entries, hash)
		return false
	}
	return true
}

func (c *imageSafetyRequestCache) add(hash uint64, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[uint64]time.Time)
	}
	ttl := c.ttl
	if ttl <= 0 {
		ttl = imageSafetyRequestCacheTTL
	}
	maxEntries := c.maxEntries
	if maxEntries <= 0 {
		maxEntries = imageSafetyRequestCacheMaxEntries
	}
	for key, expiresAt := range c.entries {
		if !now.Before(expiresAt) {
			delete(c.entries, key)
		}
	}
	if _, exists := c.entries[hash]; !exists && len(c.entries) >= maxEntries {
		var oldestHash uint64
		var oldestExpiry time.Time
		for key, expiresAt := range c.entries {
			if oldestExpiry.IsZero() || expiresAt.Before(oldestExpiry) {
				oldestHash = key
				oldestExpiry = expiresAt
			}
		}
		delete(c.entries, oldestHash)
	}
	c.entries[hash] = now.Add(ttl)
}

func imageSafetyRequestHash(req *sdk.ForwardRequest, method, path string) (uint64, bool) {
	if req == nil || !isImagesRequest(path) {
		return 0, false
	}
	var hasher maphash.Hash
	hasher.SetSeed(imageSafetyRequestHashSeed)
	writeImageSafetyHashPart := func(value []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hasher.Write(size[:])
		_, _ = hasher.Write(value)
	}
	writeImageSafetyHashPart([]byte(method))
	writeImageSafetyHashPart([]byte(path))
	writeImageSafetyHashPart([]byte(req.Model))
	writeImageSafetyHashPart([]byte(req.Headers.Get("Content-Type")))
	writeImageSafetyHashPart(req.Body)
	return hasher.Sum64(), true
}

func withImageSafetyRequestHash(ctx context.Context, hash uint64) context.Context {
	return context.WithValue(ctx, imageSafetyRequestContextKey{}, hash)
}

func imageSafetyRequestHashFromContext(ctx context.Context) (uint64, bool) {
	if ctx == nil {
		return 0, false
	}
	hash, ok := ctx.Value(imageSafetyRequestContextKey{}).(uint64)
	return hash, ok
}

func (g *OpenAIGateway) checkImageSafetyRequest(ctx context.Context, req *sdk.ForwardRequest, method, path string) (context.Context, *sdk.ForwardOutcome) {
	hash, ok := imageSafetyRequestHash(req, method, path)
	if !ok {
		return ctx, nil
	}
	if g != nil && g.imageSafety.contains(hash, time.Now()) {
		outcome := imageSafetyClientOutcome()
		return ctx, &outcome
	}
	return withImageSafetyRequestHash(ctx, hash), nil
}

func (g *OpenAIGateway) rememberImageSafetyRequest(ctx context.Context) {
	if g == nil {
		return
	}
	if hash, ok := imageSafetyRequestHashFromContext(ctx); ok {
		g.imageSafety.add(hash, time.Now())
	}
}

func (g *OpenAIGateway) rememberImageSafetyRequestHex(value string) {
	if g == nil || value == "" {
		return
	}
	hash, err := strconv.ParseUint(value, 16, 64)
	if err != nil {
		return
	}
	g.imageSafety.add(hash, time.Now())
}

func imageSafetyRequestHashHex(req *sdk.ForwardRequest, method, path string) string {
	hash, ok := imageSafetyRequestHash(req, method, path)
	if !ok {
		return ""
	}
	return strconv.FormatUint(hash, 16)
}

func imageSafetyClientOutcome() sdk.ForwardOutcome {
	body := buildImagesErrorBodyWithCode(
		http.StatusBadRequest,
		imageSafetyInvalidRequestCode,
		imageSafetyInvalidRequestMessage,
	)
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Headers: http.Header{
				"Content-Type":   []string{"application/json"},
				"Content-Length": []string{strconv.Itoa(len(body))},
			},
			Body: body,
		},
		Reason: imageSafetyInvalidRequestMessage,
	}
}
