package gateway

import (
	"encoding/base64"
	"strconv"
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

var structurallyValidEncryptedContentSink bool
var encryptedContentPreprocessBenchmarkSink []byte

func TestIsStructurallyValidGPTReasoningEncryptedContent(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest()
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "valid raw base64url", value: valid, want: true},
		{name: "valid padded base64url", value: valid + "==", want: true},
		{name: "empty", value: "", want: false},
		{name: "wrong prefix", value: "fAAAA" + valid[5:], want: false},
		{name: "invalid character", value: valid[:20] + "…" + valid[20:], want: false},
		{name: "leading whitespace", value: " " + valid, want: false},
		{name: "bad base64", value: "gAAAA!!!!", want: false},
		{name: "short decoded payload", value: "gAAAAA", want: false},
		{name: "internal padding", value: valid[:20] + "=" + valid[20:], want: false},
		{name: "excessive padding", value: valid + "===", want: false},
		{name: "wrong padding count", value: valid + "=", want: false},
		{name: "invalid base64 remainder", value: valid[:len(valid)-1], want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStructurallyValidGPTReasoningEncryptedContent(tc.value); got != tc.want {
				t.Fatalf("valid(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestIsStructurallyValidGPTReasoningEncryptedContentDoesNotAllocate(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest() + "=="
	allocations := testing.AllocsPerRun(100, func() {
		structurallyValidEncryptedContentSink = isStructurallyValidGPTReasoningEncryptedContent(valid)
	})
	if allocations != 0 {
		t.Fatalf("structural validation allocations = %.2f, want 0", allocations)
	}
}

func TestSanitizeResponsesReasoningEncryptedContentDropsMalformedValues(t *testing.T) {
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
	for index := 0; index < 4; index++ {
		path := "input." + strconv.Itoa(index)
		if gjson.GetBytes(got, path+".encrypted_content").Exists() {
			t.Fatalf("input[%d] invalid encrypted_content still exists: %s", index, got)
		}
		if gjson.GetBytes(got, path+".id").Exists() {
			t.Fatalf("input[%d] orphan reasoning id still exists: %s", index, got)
		}
	}
	if gotID := gjson.GetBytes(got, "input.4.id").String(); gotID != "rs_good" {
		t.Fatalf("valid reasoning id = %q, want rs_good; body=%s", gotID, got)
	}
	if gotValue := gjson.GetBytes(got, "input.4.encrypted_content").String(); gotValue != valid {
		t.Fatalf("valid encrypted_content = %q, want preserved value", gotValue)
	}
	if gotID := gjson.GetBytes(got, "input.5.id").String(); gotID != "msg_1" {
		t.Fatalf("non-reasoning id = %q, want msg_1; body=%s", gotID, got)
	}
}

func TestSanitizeResponsesReasoningEncryptedContentPreservesStoreIDs(t *testing.T) {
	body := []byte(`{"store":true,"input":[` +
		`{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]},` +
		`{"id":"rs_orphan","type":"reasoning","summary":[]}` +
		`]}`)

	got := sanitizeResponsesReasoningEncryptedContent(body)
	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid encrypted_content still exists: %s", got)
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

func TestRemoveResponsesReasoningEncryptedContentForRetryDropsValidCiphertext(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForTest()
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
	if gjson.GetBytes(got, "input.encrypted_content").Exists() {
		t.Fatalf("invalid encrypted_content was not removed from input object: %s", got)
	}
	if gjson.GetBytes(got, "input.id").Exists() {
		t.Fatalf("orphan reasoning id was not removed from input object: %s", got)
	}
}

func TestSanitizeResponsesWebSocketClientMessageOnlyTouchesResponseCreate(t *testing.T) {
	invalid := `{"type":"reasoning","id":"rs_bad","encrypted_content":"bad"}`
	responseCreate := []byte(`{"type":"response.create","store":false,"input":[` + invalid + `]}`)
	got := sanitizeResponsesWebSocketClientMessage(responseCreate)
	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("response.create invalid encrypted_content still exists: %s", got)
	}
	if gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("response.create orphan id still exists: %s", got)
	}

	other := []byte(`{"type":"session.update","input":[` + invalid + `]}`)
	if gotOther := sanitizeResponsesWebSocketClientMessage(other); string(gotOther) != string(other) {
		t.Fatalf("non-response.create message was changed: %s", gotOther)
	}
}

func TestPreprocessRequestBodySanitizesMalformedReasoningEncryptedContent(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"id":"rs_bad","type":"reasoning","encrypted_content":"bad","summary":[]},{"type":"message","role":"user","content":"hi"}]}`)
	got := preprocessRequestBody(body, "gpt-5.4", "/v1/responses")
	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("preprocess retained malformed encrypted_content: %s", got)
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
