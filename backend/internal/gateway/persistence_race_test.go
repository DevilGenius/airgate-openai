package gateway

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var persistenceTestDriverID atomic.Int64

type persistenceTestDriver struct {
	exec func(context.Context, []driver.NamedValue) error
}

func (d persistenceTestDriver) Open(string) (driver.Conn, error) {
	return persistenceTestConn(d), nil
}

type persistenceTestConn struct {
	exec func(context.Context, []driver.NamedValue) error
}

func (c persistenceTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c persistenceTestConn) Close() error { return nil }
func (c persistenceTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}
func (c persistenceTestConn) ExecContext(ctx context.Context, _ string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.exec(ctx, args); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

func persistenceFixtureDB(t *testing.T, exec func(context.Context, []driver.NamedValue) error) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("persistence_review_%d", persistenceTestDriverID.Add(1))
	sql.Register(name, persistenceTestDriver{exec})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestFlushCannotDeleteSnapshotEnqueuedWhileWriting(t *testing.T) {
	for _, session := range []bool{false, true} {
		t.Run(fmt.Sprint(session), func(t *testing.T) {
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var calls atomic.Int64
			written := make(chan time.Time, 2)
			db := persistenceFixtureDB(t, func(_ context.Context, args []driver.NamedValue) error {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
				}
				index := 3
				if session {
					index = 9
				}
				written <- args[index].Value.(time.Time)
				return nil
			})
			store := &codexUsagePersistenceStore{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), flushCh: make(chan int64, 1), sessionFlushCh: make(chan string, 1)}
			first := time.Now().UTC()
			second := first.Add(time.Second)
			save := func(at time.Time) {
				if session {
					store.SaveSessionStateAsync(&openAISessionState{SessionKey: "test", LastUpdatedAt: at, LastSeenAt: at})
				} else {
					store.SaveAsync(7, &CodexUsageSnapshot{CapturedAt: at})
				}
			}
			flush := func() {
				if session {
					store.flushSessionStateRecord(t.Context(), "test")
				} else {
					store.flushAccount(t.Context(), 7)
				}
			}
			save(first)
			go func() { defer close(done); flush() }()
			<-entered
			save(second)
			save(first) // A late producer must not overwrite the newer pending value.
			close(release)
			<-done
			flush()
			if len(written) != 2 {
				t.Fatalf("newer snapshot lost: writes=%d", len(written))
			}
			<-written
			if at := <-written; !at.Equal(second) {
				t.Fatalf("latest write = %v", at)
			}
		})
	}
}

func TestSessionMutationIsAtomic(t *testing.T) {
	store := newSessionStateMemoryStore(time.Hour, 100)
	var workers sync.WaitGroup
	for range 128 {
		workers.Go(func() { store.Update("shared", func(state *openAISessionState) { state.AccountID++ }) })
	}
	workers.Wait()
	raw, ok := store.Load("shared")
	if !ok {
		t.Fatal("state missing")
	}
	state := raw.(*openAISessionState)
	if state.AccountID != 128 {
		t.Fatalf("lost mutations: %d", state.AccountID)
	}
	store.Store("shared", &openAISessionState{SessionKey: "shared", AccountID: 1, LastSeenAt: state.LastSeenAt.Add(-time.Second)})
	raw, _ = store.Load("shared")
	if raw.(*openAISessionState).AccountID != 128 {
		t.Fatal("stale store overwrote current state")
	}
}

func TestSessionUpdateTimestampStrictlyIncreasesAtDatabasePrecision(t *testing.T) {
	store := newSessionStateMemoryStore(time.Hour, 10)
	previous := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	store.Store("clock", &openAISessionState{SessionKey: "clock", LastSeenAt: previous, LastUpdatedAt: previous})
	for range 100 {
		state := store.Update("clock", func(state *openAISessionState) { state.LastResponseID = "new" })
		if !state.LastUpdatedAt.Truncate(time.Microsecond).After(previous) {
			t.Fatal("database timestamp did not advance")
		}
		previous = state.LastUpdatedAt
	}
}
