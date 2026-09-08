package gateway

import (
	"time"

	"github.com/zeebo/xxh3"
)

const (
	encryptedContentCacheTTL        = 24 * time.Hour
	encryptedContentCacheMaxEntries = 100_000
	encryptedContentHashDomain      = "airgate:encrypted-content:xxh3-64:v2"
)

// enabledEncryptedContentHashSession owns the complete encrypted_content
// lifecycle for one request: hash each opaque value once, remember cache hits
// in traversal order, remove those hits during rewriting, and reuse the same
// hashes if the upstream returns a Prompt or Cyber rejection.
type enabledEncryptedContentHashSession struct {
	cache        *safetyRequestCache
	checkedAt    time.Time
	scope        uint64
	candidates   []uint64
	matches      []bool
	removalIndex int
	sanitized    bool
}

func newEncryptedContentHashSession(cache *safetyRequestCache, scope uint64) encryptedContentHashSession {
	if cache == nil {
		return disabledEncryptedContent
	}
	return &enabledEncryptedContentHashSession{
		cache:     cache,
		checkedAt: time.Now(),
		scope:     scope,
	}
}

func (s *enabledEncryptedContentHashSession) BeginRewrite() {
	if s == nil {
		return
	}
	s.candidates = s.candidates[:0]
	s.matches = s.matches[:0]
	s.removalIndex = 0
	s.sanitized = false
}

func (s *enabledEncryptedContentHashSession) Inspect(raw string) {
	if s == nil {
		return
	}
	hash := encryptedContentHashWithScope(s.scope, raw)
	s.candidates = append(s.candidates, hash)
	s.matches = append(s.matches, s.cache != nil && s.cache.contains(hash, s.checkedAt))
}

func (s *enabledEncryptedContentHashSession) ShouldRemove(string) bool {
	if s == nil {
		return false
	}
	index := s.removalIndex
	s.removalIndex++
	if index < len(s.matches) && s.matches[index] {
		s.sanitized = true
		return true
	}
	return false
}

func (s *enabledEncryptedContentHashSession) Sanitized() bool {
	return s != nil && s.sanitized
}

func (s *enabledEncryptedContentHashSession) CacheViolation(rejection explicitUpstreamError) bool {
	if s == nil || s.cache == nil || len(s.candidates) == 0 {
		return false
	}
	switch normalizeExplicitUpstreamErrorCode(rejection.Code) {
	case promptUsagePolicyErrorCode, cybersecurityRiskErrorCode:
	default:
		return false
	}
	// Safety rejections concern the request rather than an individual token.
	// Cache only ciphertexts that were actually forwarded, keeping unrelated
	// fresh ciphertexts out of subsequent removal decisions.
	hashes := make([]uint64, 0, len(s.candidates))
	for index, hash := range s.candidates {
		if !s.matches[index] {
			hashes = append(hashes, hash)
		}
	}
	if len(hashes) == 0 {
		return false
	}
	s.cache.addHashesWithLimits(
		hashes,
		time.Now(),
		encryptedContentCacheTTL,
		encryptedContentCacheMaxEntries,
	)
	return true
}

func encryptedContentHashWithScope(scope uint64, raw string) uint64 {
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, encryptedContentHashDomain)
	writeXXH3HashUint64(&hasher, scope)
	writeXXH3HashStringPart(&hasher, raw)
	return hasher.Sum64()
}
