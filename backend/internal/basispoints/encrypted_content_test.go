package basispoints

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"testing"
)

func TestPreparedEncryptedContentPreservesNativeMessagesAndToolOutputs(t *testing.T) {
	for _, kind := range []string{"message", "agent_message", "function_call_output", "custom_tool_call_output"} {
		t.Run(kind, func(t *testing.T) {
			source := testSource()
			parts := []any{object{"type": "input_text", "text": "message header"}, object{"type": "encrypted_content", "encrypted_content": "opaque\r\nexact \tbytes"}}
			item := object{"type": kind, "id": "msg_fixture", "role": "user", "author": "/root", "recipient": "/root/worker", "content": parts}
			input := []any{item}
			field := "content"
			if kind == "function_call_output" || kind == "custom_tool_call_output" {
				field = "output"
				item = object{"type": kind, "call_id": "call_fixture", "output": parts}
				call := object{"type": "function_call", "name": "fixture", "call_id": "call_fixture", "arguments": `{}`}
				source["tools"] = []any{object{"type": "function", "name": "fixture"}}
				if kind == "custom_tool_call_output" {
					call["type"] = "custom_tool_call"
					delete(call, "arguments")
					call["input"] = "inspect"
					source["tools"] = []any{object{"type": "custom", "name": "fixture"}}
				}
				input = []any{call, item}
			}
			source["input"] = input
			raw, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			before := bytes.Clone(raw)
			prepared, err := PrepareRequest(raw, "encrypted-regression")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(prepared.Body())
			if err != nil {
				t.Fatal(err)
			}
			var wire object
			if err := decode(body, &wire); err != nil {
				t.Fatal(err)
			}
			items := wire["input"].([]any)
			got := items[len(items)-1].(object)
			if !reflect.DeepEqual(got[field], parts) || !bytes.Equal(before, raw) {
				t.Fatal("encrypted content changed or input mutated")
			}
			if field == "content" && !reflect.DeepEqual(got, item) {
				t.Fatal("native encrypted message envelope was lowered")
			}
		})
	}
}
