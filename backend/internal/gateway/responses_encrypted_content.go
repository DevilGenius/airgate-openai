package gateway

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// sanitizeResponsesReasoningEncryptedContent preserves encrypted reasoning as
// opaque context for subsequent Responses requests. Only ciphertexts cached
// after an explicit Prompt or Cyber rejection may be removed.
// https://developers.openai.com/api/docs/guides/reasoning#preserve-reasoning-without-stored-responses
//
// The common path returns the original body slice without rebuilding JSON.
func sanitizeResponsesReasoningEncryptedContent(body []byte) []byte {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"encrypted_content"`)) {
		return body
	}
	return sanitizeResponsesReasoningEncryptedContentKnownPresent(body)
}

func sanitizeResponsesReasoningEncryptedContentKnownPresent(body []byte) []byte {
	return sanitizeResponsesReasoningEncryptedContentKnownPresentWithState(body, disabledEncryptedContent)
}

func sanitizeResponsesReasoningEncryptedContentKnownPresentWithState(body []byte, session encryptedContentHashSession) []byte {
	stripOrphanReasoningIDs := !gjson.GetBytes(body, "store").Bool()
	if session == nil {
		session = disabledEncryptedContent
	}
	session.BeginRewrite()
	// Inspect every ciphertext once, then use the same cached decision during
	// rewriting. Only Prompt and Cyber rejections populate this cache.
	inspectResponsesReasoningEncryptedContentKnownPresent(body, session)
	policy := reasoningEncryptedContentRewritePolicy{
		stripExistingOrphanIDs: stripOrphanReasoningIDs,
		stripRemovedContentID:  stripOrphanReasoningIDs,
		shouldRemove:           session.ShouldRemove,
	}
	updated, _ := rewriteResponsesReasoningEncryptedContentKnownPresent(body, policy)
	return updated
}

// removeResponsesReasoningEncryptedContentForRetry drops only reasoning
// encrypted_content strings selected by shouldRemove after a safety rejection.
func removeResponsesReasoningEncryptedContentForRetry(body []byte, shouldRemove func(string) bool) ([]byte, bool) {
	if len(body) == 0 || shouldRemove == nil || !bytes.Contains(body, []byte(`"encrypted_content"`)) {
		return body, false
	}
	return rewriteResponsesReasoningEncryptedContentKnownPresent(body, reasoningEncryptedContentRewritePolicy{
		shouldRemove:          shouldRemove,
		stripRemovedContentID: true,
	})
}

type reasoningEncryptedContentRewritePolicy struct {
	removeAll              bool // Used only to exclude ciphertext from the prompt hash.
	stripExistingOrphanIDs bool
	stripRemovedContentID  bool
	shouldRemove           func(string) bool
}

func rewriteResponsesReasoningEncryptedContentKnownPresent(body []byte, policy reasoningEncryptedContentRewritePolicy) ([]byte, bool) {
	input := gjson.Get(readOnlyBytesString(body), "input")
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

func inspectResponsesReasoningEncryptedContentKnownPresent(body []byte, session encryptedContentHashSession) {
	if session == nil {
		return
	}
	input := gjson.Get(readOnlyBytesString(body), "input")
	if !input.Exists() {
		return
	}
	if input.IsArray() {
		for _, item := range input.Array() {
			inspectResponsesReasoningEncryptedContentItem(item, session)
		}
		return
	}
	if input.IsObject() {
		inspectResponsesReasoningEncryptedContentItem(input, session)
	}
}

func inspectResponsesReasoningEncryptedContentItem(item gjson.Result, session encryptedContentHashSession) {
	if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
		return
	}
	encryptedContent := item.Get("encrypted_content")
	if encryptedContent.Type != gjson.String {
		return
	}
	raw := encryptedContent.String()
	if raw != "" {
		session.Inspect(raw)
	}
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

	raw := encryptedContent.String()
	remove := policy.removeAll
	if !remove && encryptedContent.Type == gjson.String && raw != "" && policy.shouldRemove != nil {
		remove = policy.shouldRemove(raw)
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

func sanitizeResponsesWebSocketClientMessage(message []byte, opts responsesNormalizeOptions) []byte {
	if len(message) == 0 {
		return message
	}
	if strings.TrimSpace(gjson.GetBytes(message, "type").String()) != "response.create" {
		return message
	}
	if strings.TrimSpace(opts.model) == "" {
		opts.model = gjson.GetBytes(message, "model").String()
	}
	opts.finalize = true
	return normalizeResponsesInputWithOptions(message, "/v1/responses", opts)
}
