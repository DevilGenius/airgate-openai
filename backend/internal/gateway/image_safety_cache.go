package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const (
	imageSafetyRequestCacheTTL        = 10 * time.Minute
	imageSafetyRequestCacheMaxEntries = 8192
	imageSafetyRequestHashInputKey    = "_image_safety_request_hash"
	imageSafetyRequestHashSeed        = uint64(0x9e3779b185ebca87)
	imageSafetyMultipartHashSeed      = uint64(0xc2b2ae3d27d4eb4f)
)

type imageSafetyRequestContextKey struct{}

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
	rawContentType := strings.TrimSpace(req.Headers.Get("Content-Type"))
	contentType := strings.ToLower(rawContentType)
	boundary := ""
	if strings.Contains(rawContentType, ";") {
		if mediaType, params, err := mime.ParseMediaType(rawContentType); err == nil {
			contentType = strings.ToLower(strings.TrimSpace(mediaType))
			boundary = params["boundary"]
		}
	}
	var hasher xxhash.Digest
	hasher.ResetWithSeed(imageSafetyRequestHashSeed)
	writeImageSafetyHashPart(&hasher, []byte(method))
	writeImageSafetyHashPart(&hasher, []byte(path))
	writeImageSafetyHashPart(&hasher, []byte(req.Model))
	writeImageSafetyHashPart(&hasher, []byte(contentType))
	if isImagesEditRequest(path) && contentType == "multipart/form-data" {
		if multipartDigest, err := imageSafetyMultipartDigest(req.Body, boundary); err == nil {
			writeImageSafetyHashPart(&hasher, []byte("multipart"))
			writeImageSafetyHashUint64(&hasher, multipartDigest)
			return hasher.Sum64(), true
		}
	}
	writeImageSafetyHashPart(&hasher, []byte("raw"))
	writeImageSafetyHashUint64(&hasher, uint64(len(req.Body)))
	writeImageSafetyHashUint64(&hasher, xxhash.Sum64(req.Body))
	return hasher.Sum64(), true
}

func imageSafetyMultipartDigest(body []byte, boundary string) (uint64, error) {
	if boundary == "" {
		return 0, fmt.Errorf("multipart content-type 缺少 boundary")
	}
	var delimiterStorage [72]byte
	var delimiter []byte
	if len(boundary) <= len(delimiterStorage)-2 {
		delimiter = delimiterStorage[:len(boundary)+2]
		copy(delimiter, "--")
		copy(delimiter[2:], boundary)
	} else {
		delimiter = append([]byte("--"), boundary...)
	}
	var hasher xxhash.Digest
	hasher.ResetWithSeed(imageSafetyMultipartHashSeed)
	searchFrom := 0
	writtenFrom := 0
	foundBoundary := false
	for {
		relative := bytes.Index(body[searchFrom:], delimiter)
		if relative < 0 {
			break
		}
		position := searchFrom + relative
		afterDelimiter := position + len(delimiter)
		if !isImageSafetyMultipartDelimiter(body, position, afterDelimiter) {
			searchFrom = position + 1
			continue
		}
		_, _ = hasher.Write(body[writtenFrom:position])
		_, _ = hasher.WriteString("--<boundary>")
		writtenFrom = afterDelimiter
		searchFrom = afterDelimiter
		foundBoundary = true
	}
	if !foundBoundary {
		return 0, fmt.Errorf("multipart body 中未找到 boundary")
	}
	_, _ = hasher.Write(body[writtenFrom:])
	return hasher.Sum64(), nil
}

func isImageSafetyMultipartDelimiter(body []byte, position, afterDelimiter int) bool {
	validPrefix := position == 0 ||
		(position >= 2 && body[position-2] == '\r' && body[position-1] == '\n')
	if !validPrefix || afterDelimiter+2 > len(body) {
		return false
	}
	return (body[afterDelimiter] == '\r' && body[afterDelimiter+1] == '\n') ||
		(body[afterDelimiter] == '-' && body[afterDelimiter+1] == '-')
}

func writeImageSafetyHashPart(dst *xxhash.Digest, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = dst.Write(size[:])
	_, _ = dst.Write(value)
}

func writeImageSafetyHashUint64(dst *xxhash.Digest, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = dst.Write(encoded[:])
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
