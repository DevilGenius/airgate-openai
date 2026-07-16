package gateway

import (
	"bytes"
	"encoding/base64"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxGPTReasoningEncryptedContentLen = 32 * 1024 * 1024

// sanitizeResponsesReasoningEncryptedContent removes reasoning
// encrypted_content values whose Fernet-like transport envelope is malformed.
// It only validates the outer shape; a structurally valid value may still be
// expired or otherwise undecryptable by the upstream.
//
// The common path returns the original body slice without rebuilding JSON.
func sanitizeResponsesReasoningEncryptedContent(body []byte) []byte {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"encrypted_content"`)) {
		return body
	}
	return sanitizeResponsesReasoningEncryptedContentKnownPresent(body)
}

func sanitizeResponsesReasoningEncryptedContentKnownPresent(body []byte) []byte {
	stripOrphanReasoningIDs := !gjson.GetBytes(body, "store").Bool()
	updated, _ := rewriteResponsesReasoningEncryptedContentKnownPresent(body, reasoningEncryptedContentRewritePolicy{
		removeInvalid:          true,
		stripExistingOrphanIDs: stripOrphanReasoningIDs,
		stripRemovedContentID:  stripOrphanReasoningIDs,
	})
	return updated
}

// removeResponsesReasoningEncryptedContentForRetry drops only structurally
// valid reasoning encrypted_content values selected by shouldRemove. The
// caller uses this after the same ciphertext was rejected by the upstream.
func removeResponsesReasoningEncryptedContentForRetry(body []byte, shouldRemove func(string) bool) ([]byte, bool) {
	if len(body) == 0 || shouldRemove == nil || !bytes.Contains(body, []byte(`"encrypted_content"`)) {
		return body, false
	}
	return rewriteResponsesReasoningEncryptedContentKnownPresent(body, reasoningEncryptedContentRewritePolicy{
		removeValid:           shouldRemove,
		stripRemovedContentID: true,
	})
}

type reasoningEncryptedContentRewritePolicy struct {
	removeInvalid          bool
	stripExistingOrphanIDs bool
	stripRemovedContentID  bool
	removeValid            func(string) bool
}

func rewriteResponsesReasoningEncryptedContentKnownPresent(body []byte, policy reasoningEncryptedContentRewritePolicy) ([]byte, bool) {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return body, false
	}

	if input.IsArray() {
		return rewriteResponsesReasoningEncryptedContentArray(body, input, policy)
	}
	if !input.IsObject() {
		return body, false
	}

	nextItem, changed := rewriteResponsesReasoningEncryptedContentItem(input, policy)
	if !changed {
		return body, false
	}
	updated, err := sjson.SetRawBytes(body, "input", []byte(nextItem))
	if err != nil {
		return body, false
	}
	return updated, true
}

func rewriteResponsesReasoningEncryptedContentArray(body []byte, input gjson.Result, policy reasoningEncryptedContentRewritePolicy) ([]byte, bool) {
	items := input.Array()
	var rebuilt []byte
	itemsWritten := 0

	keep := func(raw string) {
		if rebuilt == nil {
			return
		}
		if itemsWritten > 0 {
			rebuilt = append(rebuilt, ',')
		}
		rebuilt = append(rebuilt, raw...)
		itemsWritten++
	}
	startRebuild := func(index int) {
		if rebuilt != nil {
			return
		}
		rebuilt = make([]byte, 0, len(input.Raw))
		rebuilt = append(rebuilt, '[')
		for i := 0; i < index; i++ {
			keep(items[i].Raw)
		}
	}

	for index, item := range items {
		nextItem, changed := rewriteResponsesReasoningEncryptedContentItem(item, policy)
		if changed {
			startRebuild(index)
			keep(nextItem)
			continue
		}
		keep(item.Raw)
	}
	if rebuilt == nil {
		return body, false
	}
	rebuilt = append(rebuilt, ']')

	updated, err := sjson.SetRawBytes(body, "input", rebuilt)
	if err != nil {
		return body, false
	}
	return updated, true
}

func rewriteResponsesReasoningEncryptedContentItem(item gjson.Result, policy reasoningEncryptedContentRewritePolicy) (string, bool) {
	if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
		return item.Raw, false
	}

	encryptedContent := item.Get("encrypted_content")
	if !encryptedContent.Exists() {
		if !policy.stripExistingOrphanIDs || !item.Get("id").Exists() {
			return item.Raw, false
		}
		nextItem, err := sjson.Delete(item.Raw, "id")
		if err != nil {
			return item.Raw, false
		}
		return nextItem, true
	}

	valid := encryptedContent.Type == gjson.String &&
		isStructurallyValidGPTReasoningEncryptedContent(encryptedContent.String())
	remove := !valid && policy.removeInvalid
	if valid && policy.removeValid != nil {
		remove = policy.removeValid(encryptedContent.String())
	}
	if !remove {
		return item.Raw, false
	}

	nextItem, err := sjson.Delete(item.Raw, "encrypted_content")
	if err != nil {
		return item.Raw, false
	}
	if policy.stripRemovedContentID && item.Get("id").Exists() {
		if withoutID, deleteErr := sjson.Delete(nextItem, "id"); deleteErr == nil {
			nextItem = withoutID
		}
	}
	return nextItem, true
}

func isStructurallyValidGPTReasoningEncryptedContent(raw string) bool {
	if raw == "" || raw != strings.TrimSpace(raw) || len(raw) > maxGPTReasoningEncryptedContentLen {
		return false
	}
	for i := 0; i < len(raw); i++ {
		char := raw[i]
		switch {
		case char >= 'A' && char <= 'Z':
		case char >= 'a' && char <= 'z':
		case char >= '0' && char <= '9':
		case char == '-' || char == '_' || char == '=':
		default:
			return false
		}
	}
	if !strings.HasPrefix(raw, "gAAAA") {
		return false
	}

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(raw)
		if err != nil {
			return false
		}
	}
	if len(decoded) < 73 || decoded[0] != 0x80 {
		return false
	}

	ciphertextLen := len(decoded) - 1 - 8 - 16 - 32
	return ciphertextLen > 0 && ciphertextLen%16 == 0
}

func sanitizeResponsesWebSocketClientMessage(message []byte) []byte {
	if len(message) == 0 || !bytes.Contains(message, []byte(`"encrypted_content"`)) {
		return message
	}
	if strings.TrimSpace(gjson.GetBytes(message, "type").String()) != "response.create" {
		return message
	}
	return sanitizeResponsesReasoningEncryptedContentKnownPresent(message)
}
