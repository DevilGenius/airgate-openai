package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/lib/pq"
)

const (
	codexUsageSnapshotTable         = "plugin_account_usage_snapshots"
	sessionStatePersistTable        = "plugin_openai_session_states"
	codexUsagePersistenceFlushEvery = 5 * time.Second
	codexUsagePersistenceQueueSize  = 1024
)

type codexUsagePersistenceStore struct {
	db       *sql.DB
	logger   *slog.Logger
	pluginID string

	flushCh        chan int64
	stopCh         chan struct{}
	sessionFlushCh chan string

	pending        sync.Map // accountID -> *CodexUsageSnapshot
	sessionPending sync.Map // sessionKey -> *openAISessionState
	wg             sync.WaitGroup
	pendingMu      sync.Mutex
	pendingSizes   map[any]int64
	pendingBytes   int64
	dropped        atomic.Int64
	closing        bool
	closeOnce      sync.Once
	runCtx         context.Context
	runCancel      context.CancelFunc
	closeCtx       context.Context
	done           chan struct{}
}

func sessionPersistKey(sessionKey string) string {
	return strings.TrimSpace(sessionKey)
}

func newCodexUsagePersistenceStore(dsn, pluginID string, logger *slog.Logger) (*codexUsagePersistenceStore, error) {
	if logger == nil {
		logger = slog.Default()
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open snapshot db: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(10 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping snapshot db: %w", err)
	}

	store := &codexUsagePersistenceStore{
		db:             db,
		logger:         logger,
		pluginID:       pluginID,
		flushCh:        make(chan int64, codexUsagePersistenceQueueSize),
		stopCh:         make(chan struct{}),
		sessionFlushCh: make(chan string, codexUsagePersistenceQueueSize),
		done:           make(chan struct{}),
	}
	if err := store.ensureSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	store.wg.Add(1)
	store.runCtx, store.runCancel = context.WithCancel(context.Background())
	go store.run()
	return store, nil
}

func (s *codexUsagePersistenceStore) ensureSchema(ctx context.Context) error {
	query := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
  plugin_id  text        NOT NULL,
  account_id bigint      NOT NULL,
  snapshot   jsonb       NOT NULL,
  captured_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT NOW(),
  created_at timestamptz NOT NULL DEFAULT NOW(),
  PRIMARY KEY (plugin_id, account_id)
);
CREATE INDEX IF NOT EXISTS idx_%s_plugin_updated_at
  ON %s (plugin_id, updated_at DESC);`,
		codexUsageSnapshotTable,
		codexUsageSnapshotTable,
		codexUsageSnapshotTable,
	)

	if _, err := s.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("ensure snapshot schema: %w", err)
	}

	sessionQuery := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
  plugin_id         text        NOT NULL,
  session_key       text        NOT NULL,
  session_id        text        NOT NULL DEFAULT '',
  conversation_id   text        NOT NULL DEFAULT '',
  prompt_cache_key  text        NOT NULL DEFAULT '',
  account_id        bigint      NOT NULL DEFAULT 0,
  last_response_id  text        NOT NULL DEFAULT '',
  last_turn_state   text        NOT NULL DEFAULT '',
  last_seen_at      timestamptz NOT NULL DEFAULT NOW(),
  last_updated_at   timestamptz NOT NULL DEFAULT NOW(),
  last_response_at  timestamptz,
  last_turn_state_at timestamptz,
  PRIMARY KEY (plugin_id, session_key)
);
CREATE INDEX IF NOT EXISTS idx_%s_updated_at
  ON %s (plugin_id, last_updated_at DESC);`,
		sessionStatePersistTable,
		sessionStatePersistTable,
		sessionStatePersistTable,
	)
	if _, err := s.db.ExecContext(ctx, sessionQuery); err != nil {
		return fmt.Errorf("ensure session state schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS account_id bigint NOT NULL DEFAULT 0`, sessionStatePersistTable)); err != nil {
		return fmt.Errorf("ensure session account_id column: %w", err)
	}
	return nil
}

func (s *codexUsagePersistenceStore) run() {
	s.runWithInterval(codexUsagePersistenceFlushEvery)
}

func (s *codexUsagePersistenceStore) runWithInterval(interval time.Duration) {
	defer s.wg.Done()
	defer close(s.done)
	defer s.db.Close()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.flushCh:
			// Coalesce snapshots until the bounded periodic batch.
		case <-s.sessionFlushCh:
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.runCtx, 5*time.Second)
			s.flushAll(ctx)
			cancel()
		case <-s.stopCh:
			ctx, cancel := context.WithTimeout(s.closeCtx, 5*time.Second)
			s.flushAll(ctx)
			cancel()
			return
		}
	}
}

func (s *codexUsagePersistenceStore) SaveAsync(accountID int64, snapshot *CodexUsageSnapshot) {
	if s == nil || snapshot == nil {
		return
	}
	cloned := cloneCodexUsageSnapshot(snapshot)
	size := int64(512 + len(cloned.PlanType) + len(cloned.LimitName) + len(cloned.ActiveLimit))
	if !s.savePending(accountID, size, func() bool {
		return storeNewerPending(&s.pending, accountID, cloned, func(v *CodexUsageSnapshot) time.Time { return v.CapturedAt })
	}) {
		return
	}

	select {
	case s.flushCh <- accountID:
	default:
		// 队列满时保留 pending 快照，交给定时 flush 合并写入。
	}
}

func (s *codexUsagePersistenceStore) flushAll(ctx context.Context) {
	if s == nil {
		return
	}
	s.pending.Range(func(key, _ any) bool {
		if ctx.Err() != nil {
			return false
		}
		accountID, ok := key.(int64)
		if ok {
			s.flushAccount(ctx, accountID)
		}
		return true
	})
	s.sessionPending.Range(func(key, _ any) bool {
		if ctx.Err() != nil {
			return false
		}
		sessionKey, ok := key.(string)
		if ok {
			s.flushSessionStateRecord(ctx, sessionKey)
		}
		return true
	})
}

func (s *codexUsagePersistenceStore) flushAccount(ctx context.Context, accountID int64) {
	if s == nil {
		return
	}
	val, ok := s.pending.Load(accountID)
	if !ok {
		return
	}
	snapshot, ok := val.(*CodexUsageSnapshot)
	if !ok || snapshot == nil {
		s.removePending(&s.pending, accountID, val)
		return
	}
	if err := s.upsert(ctx, accountID, snapshot); err != nil {
		s.logger.Warn("持久化 Codex 用量快照失败", "account_id", accountID, "error", err)
		return
	}
	s.removePending(&s.pending, accountID, val)
}

func (s *codexUsagePersistenceStore) upsert(ctx context.Context, accountID int64, snapshot *CodexUsageSnapshot) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	capturedAt := snapshot.CapturedAt.UTC()
	if capturedAt.IsZero() {
		capturedAt = time.Now().UTC()
	}

	query := fmt.Sprintf(`
INSERT INTO %s (plugin_id, account_id, snapshot, captured_at, updated_at)
VALUES ($1, $2, $3::jsonb, $4, NOW())
ON CONFLICT (plugin_id, account_id)
DO UPDATE SET
  snapshot = EXCLUDED.snapshot,
  captured_at = EXCLUDED.captured_at,
  updated_at = NOW()
WHERE %[1]s.captured_at <= EXCLUDED.captured_at`, codexUsageSnapshotTable)

	if _, err := s.db.ExecContext(ctx, query, s.pluginID, accountID, string(payload), capturedAt); err != nil {
		return fmt.Errorf("upsert snapshot: %w", err)
	}
	return nil
}

func (s *codexUsagePersistenceStore) SaveSessionStateAsync(state *openAISessionState) {
	if s == nil || state == nil {
		return
	}
	key := sessionPersistKey(state.SessionKey)
	if key == "" {
		return
	}
	cloned := cloneSessionState(state)
	size := int64(512 + len(cloned.SessionKey) + len(cloned.SessionID) + len(cloned.ConversationID) + len(cloned.PromptCacheKey) + len(cloned.LastResponseID) + len(cloned.LastTurnState))
	if !s.savePending(key, size, func() bool { return storeNewerPending(&s.sessionPending, key, cloned, sessionStateLastActivity) }) {
		return
	}
	select {
	case s.sessionFlushCh <- key:
	default:
	}
}

func (s *codexUsagePersistenceStore) flushSessionStateRecord(ctx context.Context, key string) {
	val, ok := s.sessionPending.Load(key)
	if !ok {
		return
	}
	state, ok := val.(*openAISessionState)
	if !ok || state == nil {
		s.removePending(&s.sessionPending, key, val)
		return
	}
	if err := s.upsertSessionState(ctx, state); err != nil {
		s.logger.Warn("持久化 OpenAI 会话状态失败", "session_key", key, "error", err)
		return
	}
	s.removePending(&s.sessionPending, key, val)
}

func (s *codexUsagePersistenceStore) upsertSessionState(ctx context.Context, state *openAISessionState) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if state == nil || strings.TrimSpace(state.SessionKey) == "" {
		return nil
	}
	query := fmt.Sprintf(`
INSERT INTO %s (
  plugin_id, session_key, session_id, conversation_id, prompt_cache_key,
  account_id, last_response_id, last_turn_state, last_seen_at, last_updated_at,
  last_response_at, last_turn_state_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (plugin_id, session_key)
DO UPDATE SET
  session_id = EXCLUDED.session_id,
  conversation_id = EXCLUDED.conversation_id,
  prompt_cache_key = EXCLUDED.prompt_cache_key,
  account_id = EXCLUDED.account_id,
  last_response_id = EXCLUDED.last_response_id,
  last_turn_state = EXCLUDED.last_turn_state,
  last_seen_at = EXCLUDED.last_seen_at,
  last_updated_at = EXCLUDED.last_updated_at,
  last_response_at = EXCLUDED.last_response_at,
  last_turn_state_at = EXCLUDED.last_turn_state_at
WHERE %[1]s.last_updated_at <= EXCLUDED.last_updated_at`,
		sessionStatePersistTable,
	)
	_, err := s.db.ExecContext(
		ctx,
		query,
		s.pluginID,
		strings.TrimSpace(state.SessionKey),
		strings.TrimSpace(state.SessionID),
		strings.TrimSpace(state.ConversationID),
		strings.TrimSpace(state.PromptCacheKey),
		state.AccountID,
		strings.TrimSpace(state.LastResponseID),
		strings.TrimSpace(state.LastTurnState),
		state.LastSeenAt.UTC(),
		state.LastUpdatedAt.UTC(),
		nullableUTCTime(state.LastResponseAt),
		nullableUTCTime(state.LastTurnStateAt),
	)
	if err != nil {
		return fmt.Errorf("upsert session state: %w", err)
	}
	return nil
}

func (s *codexUsagePersistenceStore) Load(ctx context.Context, accountID int64) (*CodexUsageSnapshot, error) {
	if s == nil {
		return nil, nil
	}
	query := fmt.Sprintf(`SELECT snapshot FROM %s WHERE plugin_id = $1 AND account_id = $2`, codexUsageSnapshotTable)
	var payload []byte
	err := s.db.QueryRowContext(ctx, query, s.pluginID, accountID).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load snapshot: %w", err)
	}

	var snapshot CodexUsageSnapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot: %w", err)
	}
	return &snapshot, nil
}

func (s *codexUsagePersistenceStore) WarmCache(ctx context.Context) error {
	if s == nil {
		return nil
	}
	query := fmt.Sprintf(`SELECT account_id, snapshot FROM %s WHERE plugin_id = $1`, codexUsageSnapshotTable)
	rows, err := s.db.QueryContext(ctx, query, s.pluginID)
	if err != nil {
		return fmt.Errorf("warm cache query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	count := 0
	for rows.Next() {
		var accountID int64
		var payload []byte
		if err := rows.Scan(&accountID, &payload); err != nil {
			return fmt.Errorf("warm cache scan: %w", err)
		}

		var snapshot CodexUsageSnapshot
		if err := json.Unmarshal(payload, &snapshot); err != nil {
			s.logger.Warn("跳过损坏的 Codex 用量快照", "account_id", accountID, "error", err)
			continue
		}
		usageStore.Store(accountID, &snapshot)
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("warm cache rows: %w", err)
	}

	if count > 0 {
		s.logger.Info("已预热 Codex 用量快照缓存", "count", count)
	}
	sessionCount, err := s.warmSessionStateCache(ctx)
	if err != nil {
		return err
	}
	if sessionCount > 0 {
		s.logger.Info("已预热 OpenAI 会话状态缓存", "count", sessionCount)
	}
	return nil
}

func (s *codexUsagePersistenceStore) warmSessionStateCache(ctx context.Context) (int, error) {
	query := fmt.Sprintf(`SELECT session_key, session_id, conversation_id, prompt_cache_key, account_id, last_response_id, last_turn_state, last_seen_at, last_updated_at, last_response_at, last_turn_state_at FROM %s WHERE plugin_id = $1 AND session_key LIKE 'v2:%%'`, sessionStatePersistTable)
	rows, err := s.db.QueryContext(ctx, query, s.pluginID)
	if err != nil {
		return 0, fmt.Errorf("warm session state query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	count := 0
	for rows.Next() {
		state := &openAISessionState{}
		var lastResponseAt, lastTurnStateAt sql.NullTime
		if err := rows.Scan(
			&state.SessionKey,
			&state.SessionID,
			&state.ConversationID,
			&state.PromptCacheKey,
			&state.AccountID,
			&state.LastResponseID,
			&state.LastTurnState,
			&state.LastSeenAt,
			&state.LastUpdatedAt,
			&lastResponseAt,
			&lastTurnStateAt,
		); err != nil {
			return 0, fmt.Errorf("warm session state scan: %w", err)
		}
		if lastResponseAt.Valid {
			state.LastResponseAt = lastResponseAt.Time.UTC()
		}
		if lastTurnStateAt.Valid {
			state.LastTurnStateAt = lastTurnStateAt.Time.UTC()
		}
		sessionStateStore.Store(state.SessionKey, cloneSessionState(state))
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("warm session state rows: %w", err)
	}
	return count, nil
}

func nullableUTCTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func (s *codexUsagePersistenceStore) Close() error {
	return s.CloseContext(context.Background())
}

func (s *codexUsagePersistenceStore) CloseContext(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.pendingMu.Lock()
		s.closing = true
		s.pendingMu.Unlock()
		s.closeCtx = ctx
		close(s.stopCh)
		if s.runCancel != nil {
			s.runCancel()
		}
	})
	wait, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	select {
	case <-s.done:
		return nil
	case <-wait.Done():
		return wait.Err()
	}
}

func cloneCodexUsageSnapshot(snapshot *CodexUsageSnapshot) *CodexUsageSnapshot {
	if snapshot == nil {
		return nil
	}
	cloned := *snapshot
	if cloned.CapturedAt.IsZero() {
		cloned.CapturedAt = time.Now().UTC()
	}
	return &cloned
}

var (
	codexUsagePersistenceMu sync.RWMutex
	codexUsagePersistence   *codexUsagePersistenceStore
)

func setCodexUsagePersistenceStore(store *codexUsagePersistenceStore) {
	codexUsagePersistenceMu.Lock()
	defer codexUsagePersistenceMu.Unlock()
	codexUsagePersistence = store
}

func getCodexUsagePersistenceStore() *codexUsagePersistenceStore {
	codexUsagePersistenceMu.RLock()
	defer codexUsagePersistenceMu.RUnlock()
	return codexUsagePersistence
}
