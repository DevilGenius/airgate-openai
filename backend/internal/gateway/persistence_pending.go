package gateway

import (
	"sync"
	"time"
)

// An older producer can be scheduled after a newer one. Do not let that late
// enqueue replace the newer snapshot, even when flushes are serialized.
func storeNewerPending[T any](pending *sync.Map, key any, value *T, timestamp func(*T) time.Time) bool {
	for {
		old, exists := pending.Load(key)
		if !exists {
			if _, loaded := pending.LoadOrStore(key, value); !loaded {
				return true
			}
			continue
		}
		if previous, ok := old.(*T); ok && previous != nil && timestamp(previous).After(timestamp(value)) {
			return false
		}
		if pending.CompareAndSwap(key, old, value) {
			return true
		}
	}
}
