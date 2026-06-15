package gateway

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestResolveOpenAISessionUsesPromptCacheKeyAsFallback(t *testing.T) {
	headers := http.Header{}
	resolution := resolveOpenAISession(headers, []byte(`{"prompt_cache_key":"pcache_123"}`), 101)
	if resolution.SessionKey != "pcache:pcache_123" {
		t.Fatalf("expected session key from prompt_cache_key, got %q", resolution.SessionKey)
	}
	if resolution.SessionID != "pcache_123" {
		t.Fatalf("expected session_id fallback from prompt_cache_key, got %q", resolution.SessionID)
	}
}

func TestUpstreamPromptCacheKeyKeepsMaxLengthKey(t *testing.T) {
	key := strings.Repeat("a", maxUpstreamPromptCacheKeyLength)
	if got := upstreamPromptCacheKey(key); got != key {
		t.Fatalf("upstreamPromptCacheKey changed max-length key: got %q", got)
	}
}

func TestUpstreamPromptCacheKeyHashesLongKey(t *testing.T) {
	key := strings.Repeat("a", maxUpstreamPromptCacheKeyLength+6)
	got := upstreamPromptCacheKey(key)
	sum := sha256.Sum256([]byte(key))
	want := fmt.Sprintf("%x", sum[:])
	if got != want {
		t.Fatalf("upstreamPromptCacheKey = %q, want %q", got, want)
	}
	if len(got) != maxUpstreamPromptCacheKeyLength {
		t.Fatalf("hashed prompt_cache_key length = %d, want %d", len(got), maxUpstreamPromptCacheKeyLength)
	}
}

func TestResolveOpenAISessionReadsStoredState(t *testing.T) {
	sessionStateStore.Delete("pcache:pcache_456")
	upsertSessionState(&openAISessionState{
		SessionKey:     "pcache:pcache_456",
		PromptCacheKey: "pcache_456",
		SessionID:      "pcache_456",
		AccountID:      202,
		LastResponseID: "resp_abc",
		LastTurnState:  "turn_state_xyz",
	})

	resolution := resolveOpenAISession(http.Header{}, []byte(`{"prompt_cache_key":"pcache_456"}`), 202)
	if resolution.PreviousRespID != "resp_abc" {
		t.Fatalf("expected previous response id from stored state, got %q", resolution.PreviousRespID)
	}
	if resolution.LastTurnState != "turn_state_xyz" {
		t.Fatalf("expected turn state from stored state, got %q", resolution.LastTurnState)
	}
}

func TestResolveOpenAISessionIgnoresStoredResponseFromDifferentAccount(t *testing.T) {
	sessionStateStore.Delete("pcache:pcache_account_mismatch")
	upsertSessionState(&openAISessionState{
		SessionKey:     "pcache:pcache_account_mismatch",
		PromptCacheKey: "pcache_account_mismatch",
		SessionID:      "pcache_account_mismatch",
		AccountID:      301,
		LastResponseID: "resp_wrong_account",
		LastTurnState:  "turn_state_wrong_account",
	})

	resolution := resolveOpenAISession(http.Header{}, []byte(`{"prompt_cache_key":"pcache_account_mismatch"}`), 302)
	if resolution.PreviousRespID != "" {
		t.Fatalf("expected previous response id to be ignored across accounts, got %q", resolution.PreviousRespID)
	}
	if resolution.LastTurnState != "" {
		t.Fatalf("expected turn state to be ignored across accounts, got %q", resolution.LastTurnState)
	}
	if !resolution.FromStoredState {
		t.Fatalf("expected stored state to still be detected")
	}
}

func TestUpdateSessionStateFromRequestClearsContinuationStateOnAccountChange(t *testing.T) {
	sessionStateStore.Delete("pcache:pcache_account_change")
	upsertSessionState(&openAISessionState{
		SessionKey:     "pcache:pcache_account_change",
		PromptCacheKey: "pcache_account_change",
		SessionID:      "pcache_account_change",
		AccountID:      401,
		LastResponseID: "resp_old",
		LastTurnState:  "turn_state_old",
	})

	resolution := resolveOpenAISession(http.Header{}, []byte(`{"prompt_cache_key":"pcache_account_change"}`), 402)
	updateSessionStateFromRequest(resolution, 402)

	state := getSessionState("pcache:pcache_account_change")
	if state == nil {
		t.Fatalf("expected stored state")
	}
	if state.AccountID != 402 {
		t.Fatalf("AccountID = %d, want 402", state.AccountID)
	}
	if state.LastResponseID != "" {
		t.Fatalf("expected LastResponseID to be cleared, got %q", state.LastResponseID)
	}
	if state.LastTurnState != "" {
		t.Fatalf("expected LastTurnState to be cleared, got %q", state.LastTurnState)
	}
}

func TestDeriveAnthropicPromptCacheKey_IgnoresLaterUserEphemeralChanges(t *testing.T) {
	body1 := []byte(`{
		"system":[{"type":"text","text":"stable system","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"anchor user","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":[{"type":"text","text":"assistant step","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"later user one","cache_control":{"type":"ephemeral"}}]}
		]
	}`)
	body2 := []byte(`{
		"system":[{"type":"text","text":"stable system","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"anchor user","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":[{"type":"text","text":"assistant step","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"later user two","cache_control":{"type":"ephemeral"}}]}
		]
	}`)

	k1 := deriveAnthropicPromptCacheKey(body1)
	k2 := deriveAnthropicPromptCacheKey(body2)
	if k1 == "" || k2 == "" {
		t.Fatalf("expected non-empty keys")
	}
	if k1 != k2 {
		t.Fatalf("expected stable key when only later user ephemeral content changes\nk1=%s\nk2=%s", k1, k2)
	}
}

func TestDeriveAnthropicPromptCacheKey_ChangesWhenSystemChanges(t *testing.T) {
	body1 := []byte(`{
		"system":[{"type":"text","text":"stable system one","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"anchor user","cache_control":{"type":"ephemeral"}}]}]
	}`)
	body2 := []byte(`{
		"system":[{"type":"text","text":"stable system two","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"anchor user","cache_control":{"type":"ephemeral"}}]}]
	}`)

	k1 := deriveAnthropicPromptCacheKey(body1)
	k2 := deriveAnthropicPromptCacheKey(body2)
	if k1 == "" || k2 == "" {
		t.Fatalf("expected non-empty keys")
	}
	if k1 == k2 {
		t.Fatalf("expected different keys when system ephemeral content changes")
	}
}
