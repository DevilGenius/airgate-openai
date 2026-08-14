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

func TestSafetyRequestCacheTracksCategoryCounts(t *testing.T) {
	cache := safetyRequestCache{
		ttl:        10 * time.Minute,
		maxEntries: 2,
	}
	start := time.Unix(1_700_000_000, 0)
	cache.addWithCategory(1, start, cybersecurityRiskErrorCode)
	cache.addWithCategory(2, start.Add(time.Minute), promptUsagePolicyErrorCode)

	size, capacity, counts := cache.statsWithCategoryCounts(
		start.Add(2*time.Minute),
		defaultSafetyRequestCacheMaxEntries,
		cybersecurityRiskErrorCode,
		promptUsagePolicyErrorCode,
	)
	if size != 2 || capacity != 2 || counts[0] != 1 || counts[1] != 1 {
		t.Fatalf("initial stats = size:%d cap:%d counts:%v", size, capacity, counts)
	}

	cache.addWithCategory(1, start.Add(3*time.Minute), promptUsagePolicyErrorCode)
	cache.addWithCategory(3, start.Add(4*time.Minute), cybersecurityRiskErrorCode)
	size, capacity, counts = cache.statsWithCategoryCounts(
		start.Add(5*time.Minute),
		defaultSafetyRequestCacheMaxEntries,
		cybersecurityRiskErrorCode,
		promptUsagePolicyErrorCode,
	)
	if size != 2 || capacity != 2 || counts[0] != 1 || counts[1] != 1 {
		t.Fatalf("updated stats = size:%d cap:%d counts:%v", size, capacity, counts)
	}
}
