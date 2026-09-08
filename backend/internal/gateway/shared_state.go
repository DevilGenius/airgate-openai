package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func sharedStateKey(kind, key string) string {
	return fmt.Sprintf("%s:%x", kind, sha256.Sum256([]byte(key)))
}

func sharedStateUnavailable(err error) (sdk.ForwardOutcome, error) {
	return sdk.ForwardOutcome{Kind: sdk.OutcomeUpstreamTransient, FailoverScope: sdk.FailoverScopeTerminal, Reason: "session state unavailable"}, err
}

func readSessionState(key string) (*openAISessionState, error) {
	if key == "" {
		return nil, nil
	}
	if sessionStateStore.shared == nil {
		return getSessionState(key), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return sessionStateStore.loadShared(ctx, key)
}

func (s *sessionStateMemoryStore) loadShared(ctx context.Context, key string) (*openAISessionState, error) {
	value, _, found, err := s.shared.Get(ctx, sharedStateKey("session", key))
	if err != nil {
		return nil, err
	}
	if !found {
		var recovered *openAISessionState
		if store := getCodexUsagePersistenceStore(); store != nil {
			recovered, err = store.loadSessionState(ctx, key)
			if err != nil {
				return nil, err
			}
		}
		if recovered == nil {
			return nil, nil
		}
		data, err := json.Marshal(recovered)
		if err != nil {
			return nil, err
		}
		if _, err := s.shared.CompareAndSwap(ctx, sharedStateKey("session", key), "0", string(data)); err != nil {
			return nil, err
		}
		// A retiring process may have filled the key during the database read.
		value, _, found, err = s.shared.Get(ctx, sharedStateKey("session", key))
		if err != nil || !found {
			return nil, err
		}
	}
	var state openAISessionState
	if err := json.Unmarshal([]byte(value), &state); err != nil {
		return nil, err
	}
	if s.expired(&state, time.Now().UTC()) {
		return nil, nil
	}
	return &state, nil
}

func (s *sessionStateMemoryStore) updateShared(key string, update func(*openAISessionState)) *openAISessionState {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	value, err := s.shared.Update(ctx, sharedStateKey("session", key), func(raw string) (string, error) {
		current := &openAISessionState{SessionKey: key}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), current); err != nil {
				return "", err
			}
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		if !now.After(current.LastUpdatedAt) {
			now = current.LastUpdatedAt.Add(time.Microsecond)
		}
		update(current)
		current.LastSeenAt, current.LastUpdatedAt = now, now
		data, err := json.Marshal(current)
		return string(data), err
	})
	if err != nil {
		slog.Error("session_state_update_failed", "error", err)
		return nil
	}
	var result openAISessionState
	if json.Unmarshal([]byte(value), &result) != nil {
		return nil
	}
	return &result
}

func (s *codexUsagePersistenceStore) loadSessionState(ctx context.Context, key string) (*openAISessionState, error) {
	state := &openAISessionState{SessionKey: key}
	var responseAt, turnAt sql.NullTime
	err := s.db.QueryRowContext(ctx, fmt.Sprintf("SELECT session_id, conversation_id, prompt_cache_key, account_id, last_response_id, last_turn_state, last_seen_at, last_updated_at, last_response_at, last_turn_state_at FROM %s WHERE plugin_id=$1 AND session_key=$2", sessionStatePersistTable), s.pluginID, key).Scan(
		&state.SessionID, &state.ConversationID, &state.PromptCacheKey, &state.AccountID, &state.LastResponseID, &state.LastTurnState, &state.LastSeenAt, &state.LastUpdatedAt, &responseAt, &turnAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if responseAt.Valid {
		state.LastResponseAt = responseAt.Time.UTC()
	}
	if turnAt.Valid {
		state.LastTurnStateAt = turnAt.Time.UTC()
	}
	if sessionStateLastActivity(state).Before(time.Now().Add(-sessionStateMemoryTTL)) {
		return nil, nil
	}
	return state, nil
}

func loadSharedDigest(shared sdk.RuntimeState, key string) (*anthropicDigestEntry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	value, _, found, err := shared.Get(ctx, sharedStateKey("digest", key))
	if err != nil {
		slog.Warn("session_digest_read_failed", "error", err)
		return nil, false
	}
	var entry anthropicDigestEntry
	if !found || json.Unmarshal([]byte(value), &entry) != nil {
		return nil, false
	}
	return &entry, true
}

func storeSharedDigest(shared sdk.RuntimeState, key string, entry *anthropicDigestEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data, err := json.Marshal(entry)
	if err == nil {
		_, err = shared.Update(ctx, sharedStateKey("digest", key), func(string) (string, error) { return string(data), nil })
	}
	if err != nil {
		slog.Warn("session_digest_write_failed", "error", err)
	}
}

func findSharedDigestSession(shared sdk.RuntimeState, namespace, chain string) (string, string, bool) {
	keys, chains := make([]string, 0, 64), make([]string, 0, 64)
	for chain != "" && len(keys) < 64 {
		keys = append(keys, sharedStateKey("digest", namespace+chain))
		chains = append(chains, chain)
		index := strings.LastIndex(chain, "-")
		if index < 0 {
			break
		}
		chain = chain[:index]
	}
	if len(keys) == 0 {
		return "", "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	values, err := shared.GetMany(ctx, keys)
	if err != nil {
		slog.Warn("session_digest_read_failed", "error", err)
		return "", "", false
	}
	for i, key := range keys {
		var entry anthropicDigestEntry
		if raw := values[key]; raw != "" && json.Unmarshal([]byte(raw), &entry) == nil {
			return entry.SessionID, chains[i], true
		}
	}
	return "", "", false
}
