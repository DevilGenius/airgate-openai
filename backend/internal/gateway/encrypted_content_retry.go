package gateway

import (
	"bytes"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/zeebo/xxh3"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const (
	invalidEncryptedContentRetryCacheTTL        = 24 * time.Hour
	invalidEncryptedContentRetryCacheMaxEntries = 100_000
)

// applyInvalidEncryptedContentRetry removes only reasoning ciphertexts whose
// individual XXH3 hashes were cached after an earlier upstream verification
// failure. Other request content and newly added ciphertexts do not affect a
// match and are preserved.
func (g *OpenAIGateway) applyInvalidEncryptedContentRetry(req *sdk.ForwardRequest, path string) bool {
	if g == nil || req == nil || !isResponsesRequestPath(path) {
		return false
	}
	hashes := validResponsesReasoningEncryptedContentHashes(req.Body)
	matches := g.encryptedContentRetry.matching(hashes, time.Now())
	if len(matches) == 0 {
		return false
	}

	updated, changed := removeResponsesReasoningEncryptedContentForRetry(req.Body, func(raw string) bool {
		_, ok := matches[xxh3.HashString(raw)]
		return ok
	})
	if !changed {
		return false
	}
	req.Body = updated
	return true
}

func (g *OpenAIGateway) cacheInvalidEncryptedContentRetry(req *sdk.ForwardRequest, path string) bool {
	if g == nil || req == nil || !isResponsesRequestPath(path) {
		return false
	}
	hashes := validResponsesReasoningEncryptedContentHashes(req.Body)
	if len(hashes) == 0 {
		return false
	}
	g.encryptedContentRetry.addHashesWithLimits(
		hashes,
		time.Now(),
		invalidEncryptedContentRetryCacheTTL,
		invalidEncryptedContentRetryCacheMaxEntries,
	)
	return true
}

func validResponsesReasoningEncryptedContentHashes(body []byte) []uint64 {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"encrypted_content"`)) {
		return nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return nil
	}

	var hashes []uint64
	appendItem := func(item gjson.Result) {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			return
		}
		encryptedContent := item.Get("encrypted_content")
		if encryptedContent.Type != gjson.String {
			return
		}
		raw := encryptedContent.String()
		if !isStructurallyValidGPTReasoningEncryptedContent(raw) {
			return
		}
		hashes = append(hashes, xxh3.HashString(raw))
	}

	if input.IsArray() {
		for _, item := range input.Array() {
			appendItem(item)
		}
		return hashes
	}
	if input.IsObject() {
		appendItem(input)
	}
	return hashes
}

func isInvalidEncryptedContentOutcome(outcome sdk.ForwardOutcome, err error) bool {
	if outcome.Kind == sdk.OutcomeSuccess && outcome.Upstream.StatusCode < 400 && err == nil {
		return false
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	return isEncryptedContentVerificationError(
		outcome.Reason,
		string(outcome.Upstream.Body),
		errText,
	)
}
