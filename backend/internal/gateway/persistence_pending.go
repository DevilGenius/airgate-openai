package gateway

import (
	"log/slog"
	"sync"
	"time"
)

const (
	maxPendingSnapshots     = 4096
	maxPendingSnapshotBytes = 32 << 20
	maxSnapshotBytes        = 256 << 10
)

func (s *codexUsagePersistenceStore) savePending(key any, size int64, save func() bool) bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.closing {
		return false
	}
	old, exists := s.pendingSizes[key]
	if size > maxSnapshotBytes || s.pendingBytes-old+size > maxPendingSnapshotBytes || (!exists && len(s.pendingSizes) >= maxPendingSnapshots) {
		if s.dropped.Add(1)%256 == 1 {
			slog.Warn("snapshot_pending_capacity_exhausted", "dropped", s.dropped.Load())
		}
		return false
	}
	if !save() {
		return false
	}
	if s.pendingSizes == nil {
		s.pendingSizes = make(map[any]int64)
	}
	s.pendingSizes[key] = size
	s.pendingBytes += size - old
	return true
}

func (s *codexUsagePersistenceStore) removePending(pending *sync.Map, key, value any) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if pending.CompareAndDelete(key, value) {
		s.pendingBytes -= s.pendingSizes[key]
		delete(s.pendingSizes, key)
	}
}

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
