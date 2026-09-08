package gateway

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestResponsesOutputAggregationPreservesEncryptedReplay(t *testing.T) {
	const reasoning = `{"id":"rs_1","type":"reasoning","encrypted_content":"opaque-final-ciphertext=","summary":[{"type":"summary_text","text":"summary"}],"extra":{"integer":9007199254740993}}`
	const message = `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer"}],"phase":"final_answer"}`
	const call = `{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}","status":"completed"}`
	for _, transport := range []string{"ws", "sse"} {
		for _, test := range []struct {
			name     string
			terminal string
			output   string
		}{
			{name: "empty snapshot", terminal: "response.completed", output: `,"output":[]`},
			{name: "missing snapshot", terminal: "response.completed"},
			{name: "partial snapshot", terminal: "response.completed", output: `,"output":[` + message + `]`},
			{name: "full snapshot", terminal: "response.completed", output: `,"output":[` + reasoning + `,` + message + `,` + call + `]`},
			{name: "empty ciphertext", terminal: "response.completed", output: `,"output":[{"id":"rs_1","type":"reasoning","encrypted_content":null,"summary":[]},` + message + `,` + call + `]`},
			{name: "done alias", terminal: "response.done", output: `,"output":[]`},
			{name: "max output incomplete", terminal: "response.incomplete", output: `,"output":[]`},
		} {
			t.Run(transport+"/"+test.name, func(t *testing.T) {
				terminal := fmt.Sprintf(`{"type":%q,"sequence_number":9,"response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":4,"output_tokens_details":{"reasoning_tokens":2}},"incomplete_details":{"reason":"max_output_tokens"}%s}}`, test.terminal, test.output)
				events := []string{
					`{"type":"response.created","response":{"id":"resp_1"}}`,
					`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"draft"}}`,
					`{"type":"response.output_item.done","output_index":1,"item":` + message + `}`,
					`{"type":"response.output_item.done","output_index":0,"item":` + reasoning + `}`,
					`{"type":"response.output_item.done","output_index":0,"item":` + reasoning + `}`,
					`{"type":"response.output_item.done","output_index":2,"item":` + call + `}`,
					terminal,
				}
				recorder := httptest.NewRecorder()
				writer := &sseEventWriter{w: recorder, flusher: recorder, timing: newResponseEventTiming(time.Now())}
				result := parseOutputEventsForTest(t, transport, events, writer)
				if result.Err != nil {
					t.Fatal(result.Err)
				}
				if result.OutputTokens != 4 || result.ReasoningOutputTokens != 2 {
					t.Fatalf("usage changed: %+v", result)
				}
				var streamedTerminal string
				for _, line := range strings.Split(recorder.Body.String(), "\n") {
					data := strings.TrimPrefix(line, "data: ")
					if gjson.Get(data, "type").String() == test.terminal {
						streamedTerminal = data
					}
				}
				if streamedTerminal != string(result.CompletedEventRaw) {
					t.Fatal("streamed terminal snapshot differs from buffered response")
				}
				if gjson.Get(streamedTerminal, "sequence_number").Int() != 9 {
					t.Fatal("terminal event metadata changed")
				}
				body := buildNonStreamResponses(result)
				output := gjson.GetBytes(body, "output").Array()
				if len(output) != 3 || output[0].Get("id").String() != "rs_1" || output[1].Get("id").String() != "msg_1" || output[2].Get("id").String() != "fc_1" {
					t.Fatalf("output lost ordering or gained duplicates: %s", body)
				}
				if output[0].Get("encrypted_content").String() != "opaque-final-ciphertext=" || output[0].Get("extra.integer").Raw != "9007199254740993" {
					t.Fatalf("opaque item fields were lost or changed: %s", body)
				}
				if output[1].Get("phase").String() != "final_answer" || output[2].Get("call_id").String() != "call_1" {
					t.Fatalf("replay metadata lost: %s", body)
				}
				if test.name == "full snapshot" && streamedTerminal != terminal {
					t.Fatal("complete upstream snapshot should pass through unchanged")
				}
				if test.name == "empty ciphertext" && len(output[0].Get("summary").Array()) != 0 {
					t.Fatal("existing terminal summary should take precedence")
				}
			})
		}
	}
}

func TestResponsesOutputAggregationKeepsDeltaFallbackAndIgnoresUnfinishedItems(t *testing.T) {
	for _, transport := range []string{"ws", "sse"} {
		t.Run(transport, func(t *testing.T) {
			result := parseOutputEventsForTest(t, transport, []string{
				`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"cipher","summary":[],"extra":9007199254740993}}`,
				`{"type":"response.output_text.delta","delta":"answer"}`,
				`{"type":"response.output_item.added","output_index":2,"item":{"id":"rs_unfinished","type":"reasoning","encrypted_content":"draft"}}`,
				`{"type":"response.completed","response":{"id":"resp_1","output":[]}}`,
			}, nil)
			body := buildNonStreamResponses(result)
			if gjson.GetBytes(body, "output.#").Int() != 2 || gjson.GetBytes(body, "output.0.encrypted_content").String() != "cipher" || gjson.GetBytes(body, "output.1.content.0.text").String() != "answer" {
				t.Fatalf("reasoning retention broke text fallback: %s", body)
			}
			if gjson.GetBytes(body, "output.0.extra").Raw != "9007199254740993" {
				t.Fatal("text fallback changed the retained reasoning fields")
			}
		})
	}
}

func TestResponsesOutputAggregationDoesNotChangeFailures(t *testing.T) {
	const failure = `{"type":"response.failed","response":{"error":{"code":"invalid_encrypted_content","message":"The encrypted content could not be verified."},"output":[]}}`
	for _, transport := range []string{"ws", "sse"} {
		t.Run(transport, func(t *testing.T) {
			result := parseOutputEventsForTest(t, transport, []string{
				`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"cipher"}}`,
				failure,
			}, nil)
			if result.Err == nil || len(result.CompletedEventRaw) != 0 || string(result.FailedEventRaw) != failure {
				t.Fatalf("failure was altered or treated as success: %+v", result)
			}
		})
	}
}

func parseOutputEventsForTest(t *testing.T, transport string, events []string, handler WSEventHandler) WSResult {
	t.Helper()
	if transport == "sse" {
		return ParseSSEStream(strings.NewReader("data: "+strings.Join(events, "\n\ndata: ")+"\n\n"), handler)
	}
	client, peer := wsTestPair(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, event := range events {
		if err := peer.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	return ReceiveWSResponse(ctx, client, handler)
}
