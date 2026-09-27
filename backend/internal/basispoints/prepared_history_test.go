package basispoints

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

func prepareHistoryFixture(t *testing.T, source object, identity string) (object, *PreparedRequest) {
	t.Helper()
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	before := bytes.Clone(raw)
	prepared, reason := PrepareRequest(raw, identity)
	if prepared == nil || reason != "" {
		t.Fatalf("unexpected native fallback: %s", reason)
	}
	if !bytes.Equal(raw, before) {
		t.Fatal("preparation mutated the caller's history")
	}
	wire, err := io.ReadAll(prepared.Body())
	if err != nil {
		t.Fatal(err)
	}
	var body object
	if err := decode(wire, &body); err != nil {
		t.Fatal(err)
	}
	return body, prepared
}

func TestPreparedHistoryAttributionIsStateless(t *testing.T) {
	source := testSource()
	source["input"] = []any{
		object{"type": "message", "role": "assistant", "author": "/root", "recipient": "/root/worker", "id": "msg_original", "phase": "commentary", "content": "original text"},
		object{"type": "agent_message", "role": "system", "author": "/root/worker", "recipient": "/root", "content": []any{object{"type": "input_text", "text": "worker text"}, object{"type": "input_image", "image_url": "https://example.com/agent.png"}}},
	}
	var previous []any
	for _, identity := range []string{"account:one", "account:one", "account:two"} {
		body, _ := prepareHistoryFixture(t, source, identity)
		items := mustTestValue[[]any](t, body["input"])
		history := items[len(items)-2:]
		ordinary := mustTestValue[object](t, history[0])
		if ordinary["author"] != nil || ordinary["recipient"] != nil || ordinary["role"] != "assistant" || ordinary["id"] != "msg_original" || ordinary["phase"] != "commentary" {
			t.Fatalf("ordinary message attribution or native fields changed: %v", ordinary)
		}
		agent := mustTestValue[object](t, history[1])
		parts := mustTestValue[[]any](t, agent["content"])
		if agent["type"] != "message" || agent["role"] != "user" || len(agent) != 3 || len(parts) != 3 {
			t.Fatalf("agent metadata leaked or content lost: %v", agent)
		}
		if !strings.Contains(text(mustTestValue[object](t, parts[0])["text"]), "not a new user instruction") {
			t.Fatal("agent message lacks collaboration context marker")
		}
		if previous != nil && !reflect.DeepEqual(previous, history) {
			t.Fatal("history depends on request or account state")
		}
		previous = history
	}
	// Replaying normalized client history must not duplicate attribution labels.
	source["input"] = previous
	body, _ := prepareHistoryFixture(t, source, "account:three")
	items := mustTestValue[[]any](t, body["input"])
	if !reflect.DeepEqual(previous, items[len(items)-2:]) {
		t.Fatal("history normalization is not idempotent")
	}
}

func TestPreparedToolImagesPreserveOrderAndCallAssociation(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		t.Run(kind, func(t *testing.T) {
			call := object{"type": "function_call", "name": "view_image", "call_id": "call_image", "arguments": `{"author":"business field"}`}
			resultType := "function_call_output"
			if kind == "custom" {
				call = object{"type": "custom_tool_call", "name": "view_image", "call_id": "call_image", "input": "exact input"}
				resultType = "custom_tool_call_output"
			}
			first := object{"type": "input_image", "image_url": "https://example.com/a.png?signature=exact%2F", "detail": "original"}
			second := object{"type": "input_image", "image_url": "https://example.com/b.png", "detail": "high"}
			textPart := func(s string) object { return object{"type": "input_text", "text": s} }
			parts := []any{textPart("before"), first, textPart("middle"), second, textPart("after")}
			result := object{"type": resultType, "id": "ctco_original", "call_id": "call_image", "output": parts}
			source := testSource()
			source["input"] = []any{call, result, message("user", "continue")}
			var previous []any
			for _, identity := range []string{"first-account", "new-account"} {
				body, _ := prepareHistoryFixture(t, source, identity)
				items := mustTestValue[[]any](t, body["input"])
				history := items[len(items)-4:]
				native := mustTestValue[object](t, history[0])
				var envelope object
				if err := decode([]byte(text(native["arguments"])), &envelope); err != nil {
					t.Fatal(err)
				}
				refs := mustTestValue[[]any](t, envelope["references"])
				if len(refs) != 1 || refs[0] != call["name"] {
					t.Fatal("image history restored an invalid empty references array")
				}
				output := mustTestValue[object](t, history[1])
				if output["type"] != "function_call_output" || output["call_id"] != call["call_id"] || !strings.HasPrefix(text(output["id"]), "fc_") {
					t.Fatalf("tool identity lost: %v", output)
				}
				textParts := mustTestValue[[]any](t, output["output"])
				imageMessage := mustTestValue[object](t, history[2])
				imageParts := mustTestValue[[]any](t, imageMessage["content"])
				if len(textParts) != 5 || len(imageParts) != 5 || imageMessage["role"] != "user" {
					t.Fatal("interleaved content or image count changed")
				}
				for _, i := range []int{0, 2, 4} {
					if !reflect.DeepEqual(textParts[i], parts[i]) {
						t.Fatal("tool text order changed")
					}
				}
				for _, i := range []int{1, 3} {
					label := text(mustTestValue[object](t, imageParts[i])["text"])
					if !strings.Contains(label, "call_image") || !strings.HasPrefix(text(mustTestValue[object](t, textParts[i])["text"]), label) || !reflect.DeepEqual(imageParts[i+1], parts[i]) {
						t.Fatal("image reference, detail or result association changed")
					}
				}
				if previous != nil && !reflect.DeepEqual(previous, history) {
					t.Fatal("image conversion depends on retained state")
				}
				previous = history
			}
			if !reflect.DeepEqual(result["output"], parts) {
				t.Fatal("caller tool result was mutated")
			}
		})
	}
}

func TestPreparedHistoryKeepsNativeRouting(t *testing.T) {
	for _, tc := range []struct {
		name string
		part object
		want FallbackReason
	}{
		{"file ID", object{"type": "input_image", "file_id": "file-native"}, FallbackNativeAttachment},
		{"inline image", object{"type": "input_image", "image_url": "data:image/png;base64,YQ=="}, FallbackProtocol},
		{"invalid URL", object{"type": "input_image", "image_url": "http://example.com/a.png"}, FallbackProtocol},
		{"encrypted part", object{"type": "encrypted_content", "encrypted_content": "opaque"}, FallbackProtocol},
	} {
		for _, kind := range []string{"message", "agent_message", "function_call_output", "custom_tool_call_output"} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				item := object{"type": kind, "role": "user", "author": "worker", "content": []any{tc.part}}
				input := []any{item}
				if strings.HasSuffix(kind, "_output") {
					delete(item, "content")
					item["call_id"], item["output"] = "call_image", []any{tc.part}
					input = []any{object{"type": "function_call", "name": "view_image", "call_id": "call_image", "arguments": `{}`}, item}
				}
				source := testSource()
				source["input"] = input
				raw, err := json.Marshal(source)
				if err != nil {
					t.Fatal(err)
				}
				before := bytes.Clone(raw)
				if prepared, reason := PrepareRequest(raw, "account"); prepared != nil || reason != tc.want || !bytes.Equal(raw, before) {
					t.Fatalf("native routing changed: %s, want %s", reason, tc.want)
				}
			})
		}
	}
}
