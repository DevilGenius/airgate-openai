package gateway

import (
	"context"
	"database/sql/driver"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSnapshotPendingCapsRetainExistingKeyUpdates(t *testing.T) {
	s := &codexUsagePersistenceStore{}
	at := time.Now()
	for i := range maxPendingSnapshots + 1 {
		s.SaveAsync(int64(i), &CodexUsageSnapshot{CapturedAt: at})
	}
	if len(s.pendingSizes) != maxPendingSnapshots || s.dropped.Load() != 1 {
		t.Fatal("pending entry budget not enforced")
	}
	s.SaveAsync(0, &CodexUsageSnapshot{CapturedAt: at.Add(time.Second), PrimaryUsedPercent: 12})
	raw, _ := s.pending.Load(int64(0))
	if raw.(*CodexUsageSnapshot).PrimaryUsedPercent != 12 {
		t.Fatal("existing snapshot could not coalesce at capacity")
	}
	s = &codexUsagePersistenceStore{}
	large := strings.Repeat("x", 128<<10)
	for i := range 300 {
		s.SaveSessionStateAsync(&openAISessionState{SessionKey: string(rune(1000 + i)), LastTurnState: large, LastUpdatedAt: at})
	}
	if s.pendingBytes > maxPendingSnapshotBytes || s.dropped.Load() == 0 {
		t.Fatal("pending byte budget not enforced")
	}
}

func TestPersistenceStopCancelsRunningWriteAndHasDeadline(t *testing.T) {
	entered := make(chan struct{})
	var writes atomic.Int64
	db := persistenceFixtureDB(t, func(ctx context.Context, _ []driver.NamedValue) error {
		if writes.Add(1) == 1 {
			close(entered)
		}
		<-ctx.Done()
		return ctx.Err()
	})
	runCtx, cancelRun := context.WithCancel(t.Context())
	s := &codexUsagePersistenceStore{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), stopCh: make(chan struct{}), done: make(chan struct{}), runCtx: runCtx, runCancel: cancelRun}
	s.wg.Add(1)
	go s.runWithInterval(10 * time.Millisecond)
	s.SaveAsync(1, &CodexUsageSnapshot{CapturedAt: time.Now()})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("flush did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = s.CloseContext(ctx)
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("stop ignored deadline")
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("worker remained blocked after cancellation")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before := len(s.pendingSizes)
	s.SaveAsync(2, &CodexUsageSnapshot{})
	if len(s.pendingSizes) != before {
		t.Fatal("closed store accepted a new pending snapshot")
	}
}
