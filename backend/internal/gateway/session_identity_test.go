package gateway

import (
	"net/http"
	"testing"
)

// Existing parser tests exercise raw field precedence and stored-state mechanics.
// Actual gateway entry points always call the identity-aware resolver below.
func resolveSessionFixture(headers http.Header, body []byte, accountID int64) openAISessionResolution {
	return resolveOpenAISessionWithScope(headers, body, accountID, "")
}

func identityHeaders(user, key string) http.Header {
	return http.Header{"X-Airgate-User-Id": {user}, "X-Airgate-Api-Key-Id": {key}, "Session-Id": {"shared-client-session"}}
}

func TestSessionStateAndWireHintsAreIsolatedByTrustedIdentity(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"same","metadata":{"user_id":"claimed-victim"}}`)
	a := resolveOpenAISession(identityHeaders("1", "11"), body, 7)
	updateSessionStateFromRequest(a, 7)
	updateSessionStateResponseID(a.SessionKey, "response-a", 7)
	updateSessionStateTurnState(a.SessionKey, "turn-a")
	defer sessionStateStore.Delete(a.SessionKey)
	for _, headers := range []http.Header{identityHeaders("2", "11"), identityHeaders("1", "12"), identityHeaders("bad", "11"), {}} {
		other := resolveOpenAISession(headers, body, 7)
		if other.SessionKey == a.SessionKey || other.PreviousRespID != "" || other.LastTurnState != "" {
			t.Fatal("session state crossed identity boundary")
		}
		if other.wireValue("prompt_cache_key", other.PromptCacheKey) == a.wireValue("prompt_cache_key", a.PromptCacheKey) {
			t.Fatal("wire hint crossed identity boundary")
		}
	}
	last := resolveOpenAISession(identityHeaders("1", "11"), body, 7)
	if last.PreviousRespID != "response-a" || last.LastTurnState != "turn-a" {
		t.Fatal("same identity could not resume")
	}
	anonymous := resolveOpenAISession(http.Header{}, body, 7)
	if anonymous.SessionKey != "" {
		t.Fatal("untrusted request created shared session state")
	}
}

func TestAnthropicDigestCannotMatchAnotherIdentity(t *testing.T) {
	scopeA, scopeB := "user:1:key:11", "user:2:key:11"
	saveAnthropicDigestSession(8, "digest", "session-a", "", scopeA)
	defer anthropicDigestStore.Delete(anthropicDigestNamespace(8, scopeA) + "digest")
	if _, _, found := findAnthropicDigestSession(8, "digest", scopeB); found {
		t.Fatal("digest crossed user boundary")
	}
	if sid, _, found := findAnthropicDigestSession(8, "digest", scopeA); !found || sid != "session-a" {
		t.Fatal("same identity digest missing")
	}
}
