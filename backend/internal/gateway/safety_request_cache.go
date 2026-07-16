package gateway

import (
	"strings"
	"sync"
	"time"
)

const (
	defaultSafetyRequestCacheTTL        = 24 * time.Hour
	defaultSafetyRequestCacheMaxEntries = 8192
	maxSafetyCachedReasonRunes          = 2048
)

type safetyRequestCacheEntry struct {
	expiresAt time.Time
	reason    string
	hash      uint64
	older     *safetyRequestCacheEntry
	newer     *safetyRequestCacheEntry
}

type safetyRequestCache struct {
	mu         sync.Mutex
	entries    map[uint64]*safetyRequestCacheEntry
	oldest     *safetyRequestCacheEntry
	newest     *safetyRequestCacheEntry
	ttl        time.Duration
	maxEntries int
}

func (c *safetyRequestCache) contains(hash uint64, now time.Time) bool {
	_, ok := c.lookup(hash, now)
	return ok
}

func (c *safetyRequestCache) lookup(hash uint64, now time.Time) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeExpiredLocked(now)
	entry, ok := c.entries[hash]
	if !ok {
		return "", false
	}
	return entry.reason, true
}

func (c *safetyRequestCache) stats(now time.Time) (size, capacity int) {
	return c.statsWithCapacity(now, defaultSafetyRequestCacheMaxEntries)
}

func (c *safetyRequestCache) statsWithCapacity(now time.Time, defaultCapacity int) (size, capacity int) {
	capacity = defaultCapacity
	if capacity <= 0 {
		capacity = defaultSafetyRequestCacheMaxEntries
	}
	if c == nil {
		return 0, capacity
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxEntries > 0 {
		capacity = c.maxEntries
	}
	c.purgeExpiredLocked(now)
	return len(c.entries), capacity
}

func (c *safetyRequestCache) add(hash uint64, now time.Time) {
	c.addWithReason(hash, now, "")
}

func (c *safetyRequestCache) addWithReason(hash uint64, now time.Time, reason string) {
	c.addHashesWithReasonAndLimits(
		[]uint64{hash},
		now,
		reason,
		defaultSafetyRequestCacheTTL,
		defaultSafetyRequestCacheMaxEntries,
	)
}

func (c *safetyRequestCache) addHashesWithLimits(hashes []uint64, now time.Time, defaultTTL time.Duration, defaultMaxEntries int) {
	c.addHashesWithReasonAndLimits(hashes, now, "", defaultTTL, defaultMaxEntries)
}

func (c *safetyRequestCache) addHashesWithReasonAndLimits(
	hashes []uint64,
	now time.Time,
	reason string,
	defaultTTL time.Duration,
	defaultMaxEntries int,
) {
	if c == nil || len(hashes) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[uint64]*safetyRequestCacheEntry)
	}
	ttl := c.ttl
	if ttl <= 0 {
		ttl = defaultTTL
		if ttl <= 0 {
			ttl = defaultSafetyRequestCacheTTL
		}
	}
	maxEntries := c.maxEntries
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
		if maxEntries <= 0 {
			maxEntries = defaultSafetyRequestCacheMaxEntries
		}
	}
	c.purgeExpiredLocked(now)
	reason = normalizeSafetyCachedReason(reason)
	expiresAt := now.Add(ttl)
	// All entries in one cache use the same TTL, so refresh order is also
	// expiration order. Calls normally carry monotonic time.Now values; clamp a
	// rare out-of-order concurrent timestamp to preserve the O(1) ordering.
	if c.newest != nil && expiresAt.Before(c.newest.expiresAt) {
		expiresAt = c.newest.expiresAt
	}
	for _, hash := range hashes {
		if entry, exists := c.entries[hash]; exists {
			entry.expiresAt = expiresAt
			if reason != "" {
				entry.reason = reason
			}
			c.moveToNewestLocked(entry)
			continue
		}
		for len(c.entries) >= maxEntries {
			c.evictOldestLocked()
		}
		entry := &safetyRequestCacheEntry{
			expiresAt: expiresAt,
			reason:    reason,
			hash:      hash,
		}
		c.entries[hash] = entry
		c.appendNewestLocked(entry)
	}
}

func (c *safetyRequestCache) matching(hashes []uint64, now time.Time) map[uint64]struct{} {
	if c == nil || len(hashes) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeExpiredLocked(now)
	var matches map[uint64]struct{}
	for _, hash := range hashes {
		if _, ok := c.entries[hash]; !ok {
			continue
		}
		if matches == nil {
			matches = make(map[uint64]struct{})
		}
		matches[hash] = struct{}{}
	}
	return matches
}

func (c *safetyRequestCache) purgeExpiredLocked(now time.Time) {
	for c.oldest != nil && !now.Before(c.oldest.expiresAt) {
		c.evictOldestLocked()
	}
}

func (c *safetyRequestCache) evictOldestLocked() {
	if c.oldest == nil {
		return
	}
	entry := c.oldest
	c.detachLocked(entry)
	delete(c.entries, entry.hash)
}

func (c *safetyRequestCache) moveToNewestLocked(entry *safetyRequestCacheEntry) {
	if entry == nil || c.newest == entry {
		return
	}
	c.detachLocked(entry)
	c.appendNewestLocked(entry)
}

func (c *safetyRequestCache) appendNewestLocked(entry *safetyRequestCacheEntry) {
	entry.older = c.newest
	entry.newer = nil
	if c.newest != nil {
		c.newest.newer = entry
	} else {
		c.oldest = entry
	}
	c.newest = entry
}

func (c *safetyRequestCache) detachLocked(entry *safetyRequestCacheEntry) {
	if entry.older != nil {
		entry.older.newer = entry.newer
	} else {
		c.oldest = entry.newer
	}
	if entry.newer != nil {
		entry.newer.older = entry.older
	} else {
		c.newest = entry.older
	}
	entry.older = nil
	entry.newer = nil
}

func normalizeSafetyCachedReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	runes := []rune(reason)
	if len(runes) > maxSafetyCachedReasonRunes {
		return string(runes[:maxSafetyCachedReasonRunes]) + "..."
	}
	return reason
}
