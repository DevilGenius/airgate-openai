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
	"sync/atomic"
	"time"

	"github.com/zeebo/xxh3"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const (
	imageSafetyRequestCacheTTL        = defaultSafetyRequestCacheTTL
	imageSafetyRequestCacheMaxEntries = defaultSafetyRequestCacheMaxEntries
	imageSafetyRequestHashInputKey    = "_image_safety_request_hash"
	imageSafetyRequestHashDomain      = "airgate:image-safety-request:xxh3-64:v1"
	imageSafetyMultipartHashDomain    = "airgate:image-safety-multipart:xxh3-64:v1"
)

type imageSafetyRequestContextKey struct{}
type imageSafetyRequestHashCaptureContextKey struct{}

type imageSafetyRequestHashCapture struct {
	hash  atomic.Uint64
	ready atomic.Bool
}

type imageSafetyRequestCache = safetyRequestCache

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
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, imageSafetyRequestHashDomain)
	writeXXH3HashStringPart(&hasher, method)
	writeXXH3HashStringPart(&hasher, path)
	writeXXH3HashStringPart(&hasher, req.Model)
	writeXXH3HashStringPart(&hasher, contentType)
	if isImagesEditRequest(path) && contentType == "multipart/form-data" {
		if multipartDigest, err := imageSafetyMultipartDigest(req.Body, boundary); err == nil {
			writeXXH3HashStringPart(&hasher, "multipart")
			writeXXH3HashUint64(&hasher, multipartDigest)
			return hasher.Sum64(), true
		}
	}
	writeXXH3HashStringPart(&hasher, "raw")
	writeXXH3HashUint64(&hasher, uint64(len(req.Body)))
	writeXXH3HashUint64(&hasher, xxh3.Hash(req.Body))
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
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, imageSafetyMultipartHashDomain)
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

func writeXXH3HashStringPart(dst *xxh3.Hasher, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = dst.Write(size[:])
	_, _ = dst.WriteString(value)
}

func writeXXH3HashUint64(dst *xxh3.Hasher, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = dst.Write(encoded[:])
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
	// 先发布 hash，再发布 ready；读取方先观察 ready，再读取完整 hash。
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

func (g *OpenAIGateway) checkImageSafetyRequest(ctx context.Context, req *sdk.ForwardRequest, method, path string) (context.Context, *sdk.ForwardOutcome) {
	hash, ok := imageSafetyRequestHash(req, method, path)
	if !ok {
		return ctx, nil
	}
	captureImageSafetyRequestHash(ctx, hash)
	if g != nil {
		if reason, cached := g.imageSafety.lookup(hash, time.Now()); cached {
			outcome := imageSafetyClientOutcome(reason)
			// 本地缓存命中不是新的上游拒绝，避免 Forward 再次写入并重复记录 cached 日志。
			outcome.SafetyRejected = false
			return ctx, &outcome
		}
	}
	return withImageSafetyRequestHash(ctx, hash), nil
}

func (g *OpenAIGateway) cacheImageSafetyRejection(ctx context.Context, reasons ...string) {
	reason := ""
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	if g == nil {
		return
	}
	if hash, ok := imageSafetyRequestHashFromContext(ctx); ok {
		g.imageSafety.addWithReason(hash, time.Now(), reason)
	}
}

func (g *OpenAIGateway) cacheImageSafetyRejectionHash(value string) {
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

func imageSafetyClientOutcome(reasons ...string) sdk.ForwardOutcome {
	body := buildImagesErrorBodyWithCode(
		http.StatusBadRequest,
		imageSafetyInvalidRequestCode,
		imageSafetyInvalidRequestMessage,
	)
	reason := imageSafetyInvalidRequestMessage
	if len(reasons) > 0 {
		if upstreamReason := normalizeSafetyCachedReason(reasons[0]); upstreamReason != "" {
			reason = upstreamReason
		}
	}
	return sdk.ForwardOutcome{
		Kind:           sdk.OutcomeClientError,
		SafetyRejected: true,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Headers: http.Header{
				"Content-Type":   []string{"application/json"},
				"Content-Length": []string{strconv.Itoa(len(body))},
			},
			Body: body,
		},
		Reason: reason,
	}
}
