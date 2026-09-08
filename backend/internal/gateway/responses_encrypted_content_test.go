package gateway

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func validGPTReasoningEncryptedContentForTest() string {
	return validGPTReasoningEncryptedContentForTestMarker(0x42)
}

func validGPTReasoningEncryptedContentForTestMarker(marker byte) string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for i := 9; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	payload[len(payload)-1] = marker
	return base64.RawURLEncoding.EncodeToString(payload)
}

var encryptedContentPreprocessBenchmarkSink []byte

func TestSanitizeResponsesReasoningEncryptedContentPreservesOpaqueValues(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest()
	cases := []struct {
		name  string
		value string
	}{
		{name: "raw base64url", value: valid},
		{name: "padded base64url", value: valid + "=="},
		{name: "empty", value: ""},
		{name: "different prefix", value: "fAAAA" + valid[5:]},
		{name: "unicode", value: valid[:20] + "…" + valid[20:]},
		{name: "leading whitespace", value: " " + valid},
		{name: "non-base64", value: "gAAAA!!!!"},
		{name: "short payload", value: "gAAAAA"},
		{name: "internal padding", value: valid[:20] + "=" + valid[20:]},
		{name: "excessive padding", value: valid + "==="},
		{name: "different padding", value: valid + "="},
		{name: "different remainder", value: valid[:len(valid)-1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"store":false,"input":[{"id":"rs_1","type":"reasoning","encrypted_content":"` + tc.value + `","summary":[]}]}`)
			if got := sanitizeResponsesReasoningEncryptedContent(body); string(got) != string(body) {
				t.Fatalf("opaque ciphertext changed: %s", got)
			}
		})
	}
}

func TestSanitizeResponsesReasoningEncryptedContentPreservesMalformedValues(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest()
	body := []byte(`{"store":false,"input":[` +
		`{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]},` +
		`{"id":"rs_null","type":"reasoning","encrypted_content":null,"summary":[]},` +
		`{"id":"rs_number","type":"reasoning","encrypted_content":123,"summary":[]},` +
		`{"id":"rs_ws","type":"reasoning","encrypted_content":" ` + valid + ` ","summary":[]},` +
		`{"id":"rs_good","type":"reasoning","encrypted_content":"` + valid + `","summary":[]},` +
		`{"id":"msg_1","type":"message","role":"user","content":"hi"}` +
		`]}`)

	got := sanitizeResponsesReasoningEncryptedContent(body)
	if string(got) != string(body) {
		t.Fatalf("unrejected encrypted content must be preserved: %s", got)
	}
}

func TestSanitizeResponsesReasoningEncryptedContentPreservesStoreIDs(t *testing.T) {
	body := []byte(`{"store":true,"input":[` +
		`{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]},` +
		`{"id":"rs_orphan","type":"reasoning","summary":[]}` +
		`]}`)

	got := sanitizeResponsesReasoningEncryptedContent(body)
	if gotValue := gjson.GetBytes(got, "input.0.encrypted_content").String(); gotValue != "bad" {
		t.Fatalf("opaque encrypted_content = %q, want preserved", gotValue)
	}
	if gotID := gjson.GetBytes(got, "input.0.id").String(); gotID != "rs_bad" {
		t.Fatalf("store=true invalid reasoning id = %q, want rs_bad", gotID)
	}
	if gotID := gjson.GetBytes(got, "input.1.id").String(); gotID != "rs_orphan" {
		t.Fatalf("store=true orphan reasoning id = %q, want rs_orphan", gotID)
	}
}

func TestSanitizeResponsesReasoningEncryptedContentNoopKeepsOriginalBody(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest()
	body := []byte(`{"store":false,"input":[{"id":"rs_good","type":"reasoning","encrypted_content":"` + valid + `","summary":[]},{"type":"message","role":"user","content":"hi"}]}`)
	got := sanitizeResponsesReasoningEncryptedContent(body)
	if string(got) != string(body) {
		t.Fatalf("valid body changed\ngot=%s\nwant=%s", got, body)
	}
	if len(got) > 0 && &got[0] != &body[0] {
		t.Fatal("valid body should return the original slice")
	}
}

func TestRemoveResponsesReasoningEncryptedContentForRetryDropsOpaqueCiphertext(t *testing.T) {
	valid := "opaque-reasoning-content"
	body := []byte(`{"store":true,"input":[` +
		`{"id":"rs_retry","type":"reasoning","encrypted_content":"` + valid + `","summary":[{"type":"summary_text","text":"keep"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}` +
		`]}`)

	got, changed := removeResponsesReasoningEncryptedContentForRetry(body, func(raw string) bool {
		return raw == valid
	})
	if !changed {
		t.Fatal("retry cleanup should report a change")
	}
	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("retry cleanup retained encrypted_content: %s", got)
	}
	if gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("retry cleanup retained orphan reasoning id: %s", got)
	}
	if summary := gjson.GetBytes(got, "input.0.summary.0.text").String(); summary != "keep" {
		t.Fatalf("retry cleanup summary = %q, want keep; body=%s", summary, got)
	}
	if text := gjson.GetBytes(got, "input.1.content.0.text").String(); text != "answer" {
		t.Fatalf("retry cleanup changed assistant message: %s", got)
	}
}

func TestRemoveResponsesReasoningEncryptedContentForRetryNoopWithoutReasoningCiphertext(t *testing.T) {
	body := []byte(`{"input":[{"type":"compaction","encrypted_content":"opaque"},{"type":"message","role":"user","content":"hi"}]}`)
	got, changed := removeResponsesReasoningEncryptedContentForRetry(body, func(string) bool { return true })
	if changed || string(got) != string(body) {
		t.Fatalf("retry cleanup changed non-reasoning encrypted content: %s", got)
	}
}

func TestRemoveResponsesReasoningEncryptedContentForRetryOnlyDropsMatchedCiphertext(t *testing.T) {
	rejected := validGPTReasoningEncryptedContentForTestMarker(0x11)
	fresh := validGPTReasoningEncryptedContentForTestMarker(0x22)
	body := []byte(`{"store":true,"input":[` +
		`{"id":"rs_rejected","type":"reasoning","encrypted_content":"` + rejected + `","summary":[]},` +
		`{"id":"rs_fresh","type":"reasoning","encrypted_content":"` + fresh + `","summary":[]},` +
		`{"id":"rs_existing_orphan","type":"reasoning","summary":[]}` +
		`]}`)

	got, changed := removeResponsesReasoningEncryptedContentForRetry(body, func(raw string) bool {
		return raw == rejected
	})
	if !changed {
		t.Fatal("retry cleanup should remove the matched ciphertext")
	}
	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() || gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("matched ciphertext or its id was retained: %s", got)
	}
	if gotFresh := gjson.GetBytes(got, "input.1.encrypted_content").String(); gotFresh != fresh {
		t.Fatalf("unmatched ciphertext = %q, want preserved", gotFresh)
	}
	if gotID := gjson.GetBytes(got, "input.1.id").String(); gotID != "rs_fresh" {
		t.Fatalf("unmatched reasoning id = %q, want rs_fresh", gotID)
	}
	if gotID := gjson.GetBytes(got, "input.2.id").String(); gotID != "rs_existing_orphan" {
		t.Fatalf("unrelated orphan id = %q, want preserved by retry cleanup", gotID)
	}
}

func TestSanitizeResponsesReasoningEncryptedContentSupportsInputObject(t *testing.T) {
	body := []byte(`{"store":false,"input":{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]}}`)
	got := sanitizeResponsesReasoningEncryptedContent(body)
	if string(got) != string(body) {
		t.Fatalf("input object encrypted_content changed: %s", got)
	}
}

func TestSanitizeResponsesWebSocketClientMessageOnlyTouchesResponseCreate(t *testing.T) {
	invalid := `{"type":"reasoning","id":"rs_bad","encrypted_content":"bad"}`
	responseCreate := []byte(`{"type":"response.create","store":false,"input":[` + invalid + `]}`)
	got := sanitizeResponsesWebSocketClientMessage(responseCreate, responsesNormalizeOptions{strictCodex: true})
	if value := gjson.GetBytes(got, "input.0.encrypted_content").String(); value != "bad" {
		t.Fatalf("response.create encrypted_content = %q, want preserved", value)
	}
	if gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("response.create orphan id still exists: %s", got)
	}

	other := []byte(`{"type":"session.update","input":[` + invalid + `]}`)
	if gotOther := sanitizeResponsesWebSocketClientMessage(other, responsesNormalizeOptions{strictCodex: true}); string(gotOther) != string(other) {
		t.Fatalf("non-response.create message was changed: %s", gotOther)
	}
}

func TestPreprocessRequestBodyPreservesOpaqueReasoningEncryptedContent(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]},{"type":"message","role":"user","content":"hi"}]}`)
	got := preprocessRequestBody(body, "gpt-5.4", "/v1/responses")
	if value := gjson.GetBytes(got, "input.0.encrypted_content").String(); value != "bad" {
		t.Fatalf("preprocess encrypted_content = %q, want preserved", value)
	}
	if gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("preprocess retained orphan reasoning id: %s", got)
	}
	if !strings.Contains(string(got), `"store":false`) {
		t.Fatalf("preprocess did not force store=false: %s", got)
	}
}

func BenchmarkResponsesEncryptedContentPreprocess1MiB(b *testing.B) {
	body := encryptedContentBenchmarkBody()
	state := (&OpenAIGateway{}).newEncryptedContentRetryRequestState()

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for range b.N {
		encryptedContentPreprocessBenchmarkSink = preprocessRequestBodyWithEncryptedContentState(
			body,
			"gpt-5.4",
			"/v1/responses",
			state,
		)
	}
}

func encryptedContentBenchmarkBody() []byte {
	ciphertext := make([]byte, 768*1024+16)
	payload := make([]byte, 1+8+16+len(ciphertext)+32)
	payload[0] = 0x80
	encoded := base64.URLEncoding.EncodeToString(payload)
	return []byte(`{"model":"gpt-5.4","store":false,"input":[{"type":"reasoning","encrypted_content":"` + encoded + `","summary":[]}]}`)
}
