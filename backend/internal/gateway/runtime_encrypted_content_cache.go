package gateway

import (
	"strings"
	"time"

	"github.com/zeebo/xxh3"
)

const (
	encryptedContentCacheTTL        = 24 * time.Hour
	encryptedContentCacheMaxEntries = 100_000
	encryptedContentHashDomain      = "airgate:encrypted-content:xxh3-64:v2"
)

// enabledEncryptedContentHashSession owns the complete encrypted_content
// lifecycle for one request: hash each valid value once, remember cache hits
// in traversal order, remove those hits during rewriting, and reuse the same
// hashes if the upstream classifies the request as a violation.
type enabledEncryptedContentHashSession struct {
	cache        *safetyRequestCache
	checkedAt    time.Time
	scope        uint64
	candidates   []encryptedContentHashCandidate
	matches      []bool
	removalIndex int
	sanitized    bool
}

type encryptedContentHashCandidate struct {
	hash   uint64
	prefix string
	suffix string
	path   string
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

func (s *enabledEncryptedContentHashSession) Inspect(raw, path string) {
	if s == nil {
		return
	}
	hash := encryptedContentHashWithScope(s.scope, raw)
	prefix, suffix := encryptedContentMarkerParts(raw)
	s.candidates = append(s.candidates, encryptedContentHashCandidate{
		hash:   hash,
		prefix: prefix,
		suffix: suffix,
		path:   normalizeEncryptedContentParam(path),
	})
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
	if s == nil || s.cache == nil || len(s.candidates) == 0 ||
		normalizeExplicitUpstreamErrorCode(rejection.Code) != "invalid_encrypted_content" {
		return false
	}
	target, ok := s.uniqueRejectedHash(rejection)
	if !ok {
		return false
	}
	s.cache.addHashesWithLimits(
		[]uint64{target},
		time.Now(),
		encryptedContentCacheTTL,
		encryptedContentCacheMaxEntries,
	)
	return true
}

func (s *enabledEncryptedContentHashSession) uniqueRejectedHash(rejection explicitUpstreamError) (uint64, bool) {
	if param := normalizeEncryptedContentParam(rejection.Param); param != "" {
		matched := make(map[uint64]struct{})
		for _, candidate := range s.candidates {
			if candidate.path == param {
				matched[candidate.hash] = struct{}{}
			}
		}
		if len(matched) == 1 {
			for hash := range matched {
				return hash, true
			}
		}
	}

	unique := make(map[uint64]struct{}, len(s.candidates))
	for _, candidate := range s.candidates {
		unique[candidate.hash] = struct{}{}
	}
	if len(unique) == 1 {
		for hash := range unique {
			return hash, true
		}
	}

	matched := make(map[uint64]struct{})
	for _, candidate := range s.candidates {
		if encryptedContentMessageMatchesCandidate(rejection.Message, candidate) {
			matched[candidate.hash] = struct{}{}
		}
	}
	if len(matched) != 1 {
		return 0, false
	}
	for hash := range matched {
		return hash, true
	}
	return 0, false
}

func normalizeEncryptedContentParam(param string) string {
	param = strings.TrimSpace(param)
	param = strings.TrimPrefix(param, "$.")
	param = strings.ReplaceAll(param, "[", ".")
	param = strings.ReplaceAll(param, "]", "")
	return strings.Trim(param, ".")
}

func encryptedContentMarkerParts(raw string) (string, string) {
	if len(raw) < 12 {
		return raw, raw
	}
	return raw[:8], raw[len(raw)-4:]
}

func encryptedContentMessageMatchesCandidate(message string, candidate encryptedContentHashCandidate) bool {
	message = strings.TrimSpace(message)
	if message == "" || candidate.prefix == "" || candidate.suffix == "" {
		return false
	}
	prefixIndex := strings.Index(message, candidate.prefix)
	if prefixIndex < 0 {
		return false
	}
	return strings.Contains(message[prefixIndex+len(candidate.prefix):], candidate.suffix)
}

func encryptedContentHashWithScope(scope uint64, raw string) uint64 {
	var hasher xxh3.Hasher
	writeXXH3HashStringPart(&hasher, encryptedContentHashDomain)
	writeXXH3HashUint64(&hasher, scope)
	writeXXH3HashStringPart(&hasher, raw)
	return hasher.Sum64()
}
