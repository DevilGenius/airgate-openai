package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestBasispointsEncryptedAgentMessageReachesUpstreamWithoutFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
			t.Run(fmt.Sprintf("stream=%t/status=%d", stream, status), func(t *testing.T) {
				req := bpsRequest()
				req.Stream = stream
				req.Writer = httptest.NewRecorder()
				agent := map[string]any{"type": "agent_message", "id": "agent_fixture", "author": "/root", "recipient": "/root/worker",
					"content": []any{map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK\nPayload:\n"}, map[string]any{"type": "encrypted_content", "encrypted_content": "opaque-test-message"}}}
				raw, err := json.Marshal(map[string]any{"model": req.Model, "input": []any{agent}})
				if err != nil {
					t.Fatal(err)
				}
				req.Body = raw
				before := bytes.Clone(raw)
				calls := 0
				g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.URL.Path != "/basispoints/api/responses" {
						t.Error("unexpected native OAuth fallback")
					}
					var wire map[string]any
					if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					input, _ := wire["input"].([]any)
					if len(input) == 0 || !reflect.DeepEqual(input[len(input)-1], agent) {
						t.Error("encrypted message or native attribution changed")
					}
					if status != http.StatusOK {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified."}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, bpsCompleted)
				})
				outcome, used, err := g.tryBasispointsOAuth(context.Background(), req)
				if err != nil || !used || calls != 1 || !bytes.Equal(before, req.Body) {
					t.Fatalf("unexpected forwarding: used=%t calls=%d err=%v", used, calls, err)
				}
				want := sdk.OutcomeSuccess
				if status != http.StatusOK {
					want = sdk.OutcomeClientError
				}
				if outcome.Kind != want || outcome.Upstream.StatusCode != status {
					t.Fatalf("outcome kind=%s status=%d", outcome.Kind, outcome.Upstream.StatusCode)
				}
			})
		}
	}
}
