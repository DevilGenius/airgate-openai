package gateway

import (
	"testing"
	"time"
)

func TestSafetyRequestCacheRefreshMovesEntryToNewest(t *testing.T) {
	cache := safetyRequestCache{
		ttl:        10 * time.Minute,
		maxEntries: 2,
	}
	start := time.Unix(1_700_000_000, 0)
	cache.add(1, start)
	cache.add(2, start.Add(time.Minute))
	cache.add(1, start.Add(2*time.Minute))
	cache.add(3, start.Add(3*time.Minute))

	if cache.contains(2, start.Add(4*time.Minute)) {
		t.Fatal("least recently refreshed entry should be evicted")
	}
	if !cache.contains(1, start.Add(4*time.Minute)) || !cache.contains(3, start.Add(4*time.Minute)) {
		t.Fatal("refreshed and newest entries should remain cached")
	}
}
