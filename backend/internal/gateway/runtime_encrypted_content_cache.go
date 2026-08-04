package gateway

import (
	"time"

	"github.com/zeebo/xxh3"
)

const (
	encryptedContentCacheTTL        = 24 * time.Hour
	encryptedContentCacheMaxEntries = 100_000
	encryptedContentHashDomain      = "airgate:encrypted-content:xxh3-64:v1"
)

// enabledEncryptedContentHashSession owns the complete encrypted_content
// lifecycle for one request: hash each valid value once, remember cache hits
// in traversal order, remove those hits during rewriting, and reuse the same
// hashes if the upstream classifies the request as a violation.
type enabledEncryptedContentHashSession struct {
	cache        *safetyRequestCache
	checkedAt    time.Time
	validHashes  []uint64
	matches      []bool
	removalIndex int
	sanitized    bool
}

func newEncryptedContentHashSession(cache *safetyRequestCache) encryptedContentHashSession {
	if cache == nil {
		return disabledEncryptedContent
	}
	return &enabledEncryptedContentHashSession{
		cache:     cache,
		checkedAt: time.Now(),
	}
}

func (s *enabledEncryptedContentHashSession) BeginRewrite() {
	if s == nil {
		return
	}
	s.validHashes = s.validHashes[:0]
	s.matches = s.matches[:0]
	s.removalIndex = 0
	s.sanitized = false
}

func (s *enabledEncryptedContentHashSession) Inspect(raw string) {
	if s == nil {
		return
	}
	hash := encryptedContentHash(raw)
	s.validHashes = append(s.validHashes, hash)
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

func (s *enabledEncryptedContentHashSession) CacheViolation() bool {
	if s == nil || s.cache == nil || len(s.validHashes) == 0 {
		return false
	}
	s.cache.addHashesWithLimits(
		s.validHashes,
		time.Now(),
		encryptedContentCacheTTL,
		encryptedContentCacheMaxEntries,
	)
	return true
}

func encryptedContentHash(raw string) uint64 {
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, encryptedContentHashDomain)
	writeXXH3HashStringPart(&hasher, raw)
	return hasher.Sum64()
}
