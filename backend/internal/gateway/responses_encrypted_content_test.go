package gateway

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func validGPTReasoningEncryptedContentForTest() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for i := 9; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStructurallyValidGPTReasoningEncryptedContent(tc.value); got != tc.want {
				t.Fatalf("valid(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
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
