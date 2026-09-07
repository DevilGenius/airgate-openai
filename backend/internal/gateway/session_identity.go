package gateway

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

func requestSessionScope(headers http.Header) string {
	user, err := strconv.ParseInt(headers.Get("X-Airgate-User-ID"), 10, 64)
	if err != nil || user <= 0 {
		return "anonymous:" + uuid.NewString()
	}
	key := int64(0)
	if raw := headers.Get("X-Airgate-API-Key-ID"); raw != "" {
		var err error
		key, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || key < 0 {
			return "anonymous:" + uuid.NewString()
		}
	}
	return fmt.Sprintf("user:%d:key:%d", user, key)
}

func scopedSessionKey(scope, key string) string {
	if key == "" || strings.HasPrefix(scope, "anonymous:") {
		return ""
	}
	if scope == "" {
		return key
	}
	hash := sha256.Sum256([]byte(scope + "\x00" + key))
	return fmt.Sprintf("v2:%x", hash)
}

func (s openAISessionResolution) wireValue(kind, value string) string {
	if value == "" || s.Scope == "" {
		return value
	}
	hash := sha256.Sum256([]byte(s.Scope + "\x00" + kind + "\x00" + value))
	return fmt.Sprintf("%x", hash)
}

func resolveOpenAISession(headers http.Header, body []byte, accountID int64) openAISessionResolution {
	return resolveOpenAISessionWithScope(headers, body, accountID, requestSessionScope(headers))
}
