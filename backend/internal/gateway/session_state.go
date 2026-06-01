package gateway

import (
	"container/list"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

type openAISessionState struct {
	SessionKey      string    `json:"session_key"`
	SessionID       string    `json:"session_id,omitempty"`
	ConversationID  string    `json:"conversation_id,omitempty"`
	PromptCacheKey  string    `json:"prompt_cache_key,omitempty"`
	AccountID       int64     `json:"account_id,omitempty"`
	LastResponseID  string    `json:"last_response_id,omitempty"`
	LastTurnState   string    `json:"last_turn_state,omitempty"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	LastUpdatedAt   time.Time `json:"last_updated_at"`
	LastResponseAt  time.Time `json:"last_response_at"`
	LastTurnStateAt time.Time `json:"last_turn_state_at"`
}

func normalizeSessionValue(value string) string {
	return strings.TrimSpace(value)
}

func isolateSessionID(raw string) string {
	raw = normalizeSessionValue(raw)
	if raw == "" {
		return ""
	}
	return deterministicUUIDFromSeed(PluginID + ":" + raw)
}

func deterministicUUIDFromSeed(seed string) string {
	seed = normalizeSessionValue(seed)
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16],
	)
}

func sessionStateKeyFromValues(sessionID, conversationID, promptCacheKey string) string {
	if sessionID = normalizeSessionValue(sessionID); sessionID != "" {
		return "sid:" + sessionID
	}
	if conversationID = normalizeSessionValue(conversationID); conversationID != "" {
		return "cid:" + conversationID
	}
	if promptCacheKey = normalizeSessionValue(promptCacheKey); promptCacheKey != "" {
		return "pcache:" + promptCacheKey
	}
	return ""
}

func resolvePromptCacheKeyFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if key := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); key != "" {
		return key
	}
	if key := deriveAnthropicPromptCacheKey(body); key != "" {
		return key
	}
	return ""
}

func deriveAnthropicPromptCacheKey(body []byte) string {
	root := gjson.ParseBytes(body)
	if !root.Get("messages").IsArray() {
		return ""
	}

	var parts []string
	if system := root.Get("system"); system.IsArray() {
		for _, item := range system.Array() {
			if item.Get("cache_control.type").String() == "ephemeral" {
				if text := strings.TrimSpace(item.Get("text").String()); text != "" {
					parts = append(parts, "system:"+text)
				}
			}
		}
	}
	firstUserAnchor := ""
	for _, msg := range root.Get("messages").Array() {
		role := msg.Get("role").String()
		for _, item := range msg.Get("content").Array() {
			if item.Get("cache_control.type").String() == "ephemeral" {
				if text := strings.TrimSpace(item.Get("text").String()); text != "" {
					if role == "user" {
						if firstUserAnchor == "" {
							firstUserAnchor = text
						}
						continue
					}
					if role == "assistant" {
						parts = append(parts, role+":"+text)
					}
				}
			}
		}
	}
	if firstUserAnchor != "" {
		parts = append(parts, "user_anchor:"+firstUserAnchor)
	}
	if len(parts) == 0 {
		return ""
	}
	joined := strings.Join(parts, "\n")
	sum := sha256.Sum256([]byte("anthropic-cache:" + joined))
	return fmt.Sprintf("anthropic-cache-%x", sum[:16])
}

type openAISessionResolution struct {
	SessionKey      string
	SessionID       string
	ConversationID  string
	PromptCacheKey  string
	PreviousRespID  string
	LastTurnState   string
	AccountID       int64
	FromStoredState bool
	DigestChain     string
	MatchedDigest   string
	SessionSource   string
}

const (
	sessionStateMemoryTTL = 3600 * time.Second

	// sessionStateMemoryMaxEntries 只是异常流量下的高水位安全阀；正常回收主要靠 TTL。
	sessionStateMemoryMaxEntries = 1000000
	sessionStateCleanupInterval  = time.Minute
)

type sessionStateMemoryStore struct {
	mu              sync.Mutex
	items           map[string]*openAISessionState
	ttl             time.Duration
	maxEntries      int
	lastCleanupTime time.Time
}

func newSessionStateMemoryStore(ttl time.Duration, maxEntries int) *sessionStateMemoryStore {
	return &sessionStateMemoryStore{
		items:           make(map[string]*openAISessionState),
		ttl:             ttl,
		maxEntries:      maxEntries,
		lastCleanupTime: time.Now().UTC(),
	}
}

func (s *sessionStateMemoryStore) Load(key any) (any, bool) {
	sessionKey, ok := key.(string)
	if s == nil || !ok || sessionKey == "" {
		return nil, false
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(now)
	state, ok := s.items[sessionKey]
	if !ok {
		return nil, false
	}
	if s.expired(state, now) {
		delete(s.items, sessionKey)
		return nil, false
	}
	return cloneSessionState(state), true
}

func (s *sessionStateMemoryStore) Store(key, value any) {
	sessionKey, ok := key.(string)
	state, okState := value.(*openAISessionState)
	if s == nil || !ok || sessionKey == "" || !okState || state == nil {
		return
	}
	now := time.Now().UTC()
	cloned := cloneSessionState(state)
	if cloned.SessionKey == "" {
		cloned.SessionKey = sessionKey
	}
	if cloned.LastSeenAt.IsZero() {
		cloned.LastSeenAt = now
	}
	if s.expired(cloned, now) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(now)
	if _, exists := s.items[sessionKey]; !exists && s.maxEntries > 0 && len(s.items) >= s.maxEntries {
		s.deleteOldestLocked(now)
	}
	s.items[sessionKey] = cloned
}

func (s *sessionStateMemoryStore) Delete(key any) {
	sessionKey, ok := key.(string)
	if s == nil || !ok || sessionKey == "" {
		return
	}
	s.mu.Lock()
	delete(s.items, sessionKey)
	s.mu.Unlock()
}

func (s *sessionStateMemoryStore) cleanupExpiredLocked(now time.Time) {
	if now.Sub(s.lastCleanupTime) < sessionStateCleanupInterval && (s.maxEntries <= 0 || len(s.items) < s.maxEntries) {
		return
	}
	for key, state := range s.items {
		if s.expired(state, now) {
			delete(s.items, key)
		}
	}
	s.lastCleanupTime = now
}

func (s *sessionStateMemoryStore) expired(state *openAISessionState, now time.Time) bool {
	if s.ttl <= 0 || state == nil {
		return false
	}
	last := sessionStateLastActivity(state)
	return !last.IsZero() && now.Sub(last) > s.ttl
}

func (s *sessionStateMemoryStore) deleteOldestLocked(now time.Time) {
	var oldestKey string
	var oldestAt time.Time
	for key, state := range s.items {
		if s.expired(state, now) {
			delete(s.items, key)
			return
		}
		last := sessionStateLastActivity(state)
		if last.IsZero() {
			last = now
		}
		if oldestKey == "" || last.Before(oldestAt) {
			oldestKey = key
			oldestAt = last
		}
	}
	if oldestKey != "" {
		delete(s.items, oldestKey)
	}
}

func sessionStateLastActivity(state *openAISessionState) time.Time {
	if state == nil {
		return time.Time{}
	}
	last := state.LastSeenAt
	for _, candidate := range []time.Time{
		state.LastUpdatedAt,
		state.LastResponseAt,
		state.LastTurnStateAt,
	} {
		if candidate.After(last) {
			last = candidate
		}
	}
	return last.UTC()
}

func resolveOpenAISession(headers http.Header, body []byte, accountID int64) openAISessionResolution {
	promptCacheKey := resolvePromptCacheKeyFromBody(body)
	sessionID := ""
	conversationID := ""
	previousResponseID := ""
	if headers != nil {
		sessionID = strings.TrimSpace(headers.Get("session_id"))
		if sessionID == "" {
			sessionID = strings.TrimSpace(headers.Get("Session_ID"))
		}
		conversationID = strings.TrimSpace(headers.Get("conversation_id"))
		if conversationID == "" {
			conversationID = strings.TrimSpace(headers.Get("Conversation_ID"))
		}
		previousResponseID = strings.TrimSpace(headers.Get("x-openai-previous-response-id"))
	}

	sessionKey := sessionStateKeyFromValues(sessionID, conversationID, promptCacheKey)
	resolution := openAISessionResolution{
		SessionKey:     sessionKey,
		SessionID:      sessionID,
		ConversationID: conversationID,
		PromptCacheKey: promptCacheKey,
		AccountID:      accountID,
	}
	switch {
	case sessionID != "":
		resolution.SessionSource = "header_session_id"
	case conversationID != "":
		resolution.SessionSource = "header_conversation_id"
	case promptCacheKey != "":
		resolution.SessionSource = "prompt_cache_key"
	}

	if sessionKey == "" {
		return resolution
	}

	if state := getSessionState(sessionKey); state != nil {
		resolution.FromStoredState = true
		sameAccount := state.AccountID == 0 || state.AccountID == accountID
		if resolution.SessionID == "" {
			resolution.SessionID = state.SessionID
		}
		if resolution.ConversationID == "" {
			resolution.ConversationID = state.ConversationID
		}
		if resolution.PromptCacheKey == "" {
			resolution.PromptCacheKey = state.PromptCacheKey
		}
		if previousResponseID == "" && sameAccount {
			previousResponseID = state.LastResponseID
		}
		if sameAccount {
			resolution.LastTurnState = state.LastTurnState
		}
		if resolution.SessionSource == "" {
			resolution.SessionSource = "stored_session_state"
		}
	}

	if resolution.SessionID == "" && resolution.PromptCacheKey != "" {
		resolution.SessionID = resolution.PromptCacheKey
		if resolution.SessionSource == "" {
			resolution.SessionSource = "prompt_cache_key"
		}
	}
	if resolution.SessionID == "" && resolution.ConversationID != "" {
		resolution.SessionID = resolution.ConversationID
		if resolution.SessionSource == "" {
			resolution.SessionSource = "header_conversation_id"
		}
	}

	resolution.PreviousRespID = previousResponseID
	return resolution
}

var sessionStateStore = newSessionStateMemoryStore(sessionStateMemoryTTL, sessionStateMemoryMaxEntries)

const (
	anthropicDigestCacheMaxSize = 20000
	anthropicDigestCacheTTL     = time.Hour
)

var anthropicDigestStore = newAnthropicDigestCache(anthropicDigestCacheMaxSize, anthropicDigestCacheTTL)

type anthropicDigestEntry struct {
	SessionID string
	UpdatedAt time.Time
}

type anthropicDigestCacheNode struct {
	key   string
	entry *anthropicDigestEntry
}

type anthropicDigestCache struct {
	mu      sync.Mutex
	ll      *list.List
	items   map[string]*list.Element
	maxSize int
	ttl     time.Duration
}

func newAnthropicDigestCache(maxSize int, ttl time.Duration) *anthropicDigestCache {
	return &anthropicDigestCache{
		ll:      list.New(),
		items:   make(map[string]*list.Element),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

func (c *anthropicDigestCache) Load(key string) (*anthropicDigestEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	node := el.Value.(*anthropicDigestCacheNode)
	if c.ttl > 0 && time.Since(node.entry.UpdatedAt) > c.ttl {
		c.ll.Remove(el)
		delete(c.items, key)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return node.entry, true
}

func (c *anthropicDigestCache) Store(key string, entry *anthropicDigestEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*anthropicDigestCacheNode).entry = entry
		return
	}
	el := c.ll.PushFront(&anthropicDigestCacheNode{key: key, entry: entry})
	c.items[key] = el
	for c.maxSize > 0 && c.ll.Len() > c.maxSize {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*anthropicDigestCacheNode).key)
	}
}

func (c *anthropicDigestCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.Remove(el)
		delete(c.items, key)
	}
}

func shortHashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:4])
}

func buildAnthropicDigestChain(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	root := gjson.ParseBytes(body)
	var parts []string

	system := root.Get("system")
	if system.Exists() && system.Raw != "" && system.Raw != "null" {
		parts = append(parts, "s:"+shortHashBytes([]byte(system.Raw)))
	}
	for _, msg := range root.Get("messages").Array() {
		role := msg.Get("role").String()
		prefix := "u"
		if role == "assistant" {
			prefix = "a"
		}
		content := msg.Get("content").Raw
		if strings.TrimSpace(content) == "" {
			continue
		}
		parts = append(parts, prefix+":"+shortHashBytes([]byte(content)))
	}
	return strings.Join(parts, "-")
}

func anthropicDigestNamespace(accountID int64) string {
	return fmt.Sprintf("%d|", accountID)
}

func saveAnthropicDigestSession(accountID int64, digestChain, sessionID, oldDigestChain string) {
	if accountID <= 0 || digestChain == "" || sessionID == "" {
		return
	}
	ns := anthropicDigestNamespace(accountID)
	key := ns + digestChain
	anthropicDigestStore.Store(key, &anthropicDigestEntry{
		SessionID: sessionID,
		UpdatedAt: time.Now().UTC(),
	})
	if oldDigestChain != "" && oldDigestChain != digestChain {
		anthropicDigestStore.Delete(ns + oldDigestChain)
	}
}

func findAnthropicDigestSession(accountID int64, digestChain string) (sessionID string, matchedChain string, found bool) {
	if accountID <= 0 || digestChain == "" {
		return "", "", false
	}
	ns := anthropicDigestNamespace(accountID)
	chain := digestChain
	for {
		if entry, ok := anthropicDigestStore.Load(ns + chain); ok && entry != nil {
			return entry.SessionID, chain, true
		}
		i := strings.LastIndex(chain, "-")
		if i < 0 {
			return "", "", false
		}
		chain = chain[:i]
	}
}

func cloneSessionState(state *openAISessionState) *openAISessionState {
	if state == nil {
		return nil
	}
	cp := *state
	return &cp
}

func getSessionState(sessionKey string) *openAISessionState {
	if sessionKey == "" {
		return nil
	}
	val, ok := sessionStateStore.Load(sessionKey)
	if !ok {
		return nil
	}
	state, _ := val.(*openAISessionState)
	if state == nil {
		return nil
	}
	return cloneSessionState(state)
}

func upsertSessionState(state *openAISessionState) {
	if state == nil || state.SessionKey == "" {
		return
	}
	if state.LastSeenAt.IsZero() {
		state.LastSeenAt = time.Now().UTC()
	}
	cloned := cloneSessionState(state)
	sessionStateStore.Store(state.SessionKey, cloned)
	if store := getCodexUsagePersistenceStore(); store != nil {
		store.SaveSessionStateAsync(cloned)
	}
}

func touchSessionState(sessionKey string, update func(*openAISessionState)) {
	if sessionKey == "" || update == nil {
		return
	}
	now := time.Now().UTC()
	current := getSessionState(sessionKey)
	if current == nil {
		current = &openAISessionState{SessionKey: sessionKey}
	}
	current.LastSeenAt = now
	update(current)
	current.LastUpdatedAt = now
	upsertSessionState(current)
}

func updateSessionStateFromRequest(resolution openAISessionResolution, accountID int64) {
	if resolution.SessionKey == "" {
		return
	}
	touchSessionState(resolution.SessionKey, func(state *openAISessionState) {
		if state.SessionKey == "" {
			state.SessionKey = resolution.SessionKey
		}
		if sid := normalizeSessionValue(resolution.SessionID); sid != "" {
			state.SessionID = sid
		}
		if cid := normalizeSessionValue(resolution.ConversationID); cid != "" {
			state.ConversationID = cid
		}
		if pck := normalizeSessionValue(resolution.PromptCacheKey); pck != "" {
			state.PromptCacheKey = pck
		}
		if accountID > 0 {
			if state.AccountID > 0 && state.AccountID != accountID {
				state.LastResponseID = ""
				state.LastResponseAt = time.Time{}
				state.LastTurnState = ""
				state.LastTurnStateAt = time.Time{}
			}
			state.AccountID = accountID
		}
	})
}

func updateSessionStateResponseID(sessionKey, responseID string, accountID int64) {
	responseID = strings.TrimSpace(responseID)
	if sessionKey == "" || responseID == "" {
		return
	}
	now := time.Now().UTC()
	touchSessionState(sessionKey, func(state *openAISessionState) {
		if accountID > 0 {
			state.AccountID = accountID
		}
		state.LastResponseID = responseID
		state.LastResponseAt = now
	})
}

func clearSessionStateResponseID(sessionKey string) {
	if sessionKey == "" {
		return
	}
	touchSessionState(sessionKey, func(state *openAISessionState) {
		state.LastResponseID = ""
		state.LastResponseAt = time.Time{}
	})
}

func updateSessionStateTurnState(sessionKey, turnState string) {
	turnState = strings.TrimSpace(turnState)
	if sessionKey == "" || turnState == "" {
		return
	}
	now := time.Now().UTC()
	touchSessionState(sessionKey, func(state *openAISessionState) {
		state.LastTurnState = turnState
		state.LastTurnStateAt = now
	})
}

func decodeTurnStateHeader(headers http.Header) string {
	if headers == nil {
		return ""
	}
	return strings.TrimSpace(headers.Get("x-codex-turn-state"))
}
