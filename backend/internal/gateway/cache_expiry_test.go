package gateway

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestCacheCleanupDoesBoundedWorkAtHighWater(t *testing.T) {
	now := time.Now()
	pool := NewTransportPool()
	store := newSessionStateMemoryStore(time.Hour, 1000)
	for i := range 1000 {
		key := fmt.Sprint(i)
		pool.putLocked(key, &transportPoolEntry{transport: &http.Transport{}, lastUsedAt: now.Add(-3 * time.Hour)})
		store.putLocked(key, &openAISessionState{SessionKey: key, LastSeenAt: now.Add(-3 * time.Hour)})
	}
	pool.GetTransport(2000, "")
	store.Load("missing")
	if len(pool.transports) != 1001-cacheCleanupBudget || len(store.items) != 1000-cacheCleanupBudget {
		t.Fatalf("cleanup exceeded budget: transports=%d sessions=%d", len(pool.transports), len(store.items))
	}
	if len(pool.expiries) != len(pool.transports) || len(store.expiries) != len(store.items) {
		t.Fatal("expiry index lost synchronization")
	}
}

func TestSessionExpiryIndexTracksUpdatesAndEviction(t *testing.T) {
	store := newSessionStateMemoryStore(time.Hour, 2)
	now := time.Now()
	store.Store("old", &openAISessionState{SessionKey: "old", LastSeenAt: now.Add(-time.Minute)})
	store.Store("keep", &openAISessionState{SessionKey: "keep", LastSeenAt: now.Add(-time.Second)})
	store.Update("old", func(state *openAISessionState) { state.LastResponseID = "updated" })
	store.Store("new", &openAISessionState{SessionKey: "new", LastSeenAt: time.Now()})
	if _, ok := store.Load("keep"); ok {
		t.Fatal("did not evict earliest activity")
	}
	if _, ok := store.Load("old"); !ok {
		t.Fatal("updated state was evicted")
	}
	store.Delete("old")
	if len(store.expiries) != 1 || len(store.items) != 1 {
		t.Fatal("delete retained expiry node")
	}
}

func BenchmarkSessionCacheAtCapacity(b *testing.B) {
	for _, size := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			store := newSessionStateMemoryStore(time.Hour, size)
			now := time.Now()
			for i := range size {
				key := fmt.Sprint(i)
				store.Store(key, &openAISessionState{SessionKey: key, LastSeenAt: now})
			}
			b.ResetTimer()
			for range b.N {
				store.Load("0")
			}
		})
	}
}
