package basispoints

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPreparedRequestRoutesBeforeNetworkWithoutMutatingRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		extra  object
		native bool
	}{
		{name: "text"},
		{name: "functions", extra: object{"tools": []any{object{"type": "function", "name": "shell", "parameters": object{"type": "object"}}}}},
		{name: "model passthrough", extra: object{"model": "future-model"}},
		{name: "hosted search", extra: object{"tools": []any{object{"type": "web_search"}}}, native: true},
		{name: "nested hosted tool", extra: object{"tools": []any{object{"type": "namespace", "name": "nested", "tools": []any{object{"type": "web_search"}}}}}, native: true},
		{name: "additional hosted tool", extra: object{"input": []any{message("user", "hi"), object{"type": "additional_tools", "tools": []any{object{"type": "web_search"}}}}}, native: true},
		{name: "image generation", extra: object{"tools": []any{object{"type": "image_generation"}}}, native: true},
		{name: "incremental history", extra: object{"previous_response_id": "resp-native"}, native: true},
		{name: "priority", extra: object{"service_tier": "priority"}, native: true},
		{name: "sampling", extra: object{"temperature": 0.5}, native: true},
		{name: "inline image", extra: object{"input": []any{object{"role": "user", "content": []any{object{"type": "input_image", "image_url": "data:image/png;base64,YQ=="}}}}}, native: true},
		{name: "native attachment", extra: object{"input": []any{object{"role": "user", "content": []any{object{"type": "input_image", "file_id": "file-abc"}}}}}, native: true},
		{name: "encrypted message", extra: object{"input": []any{object{"role": "user", "content": []any{object{"type": "encrypted_content", "encrypted_content": "opaque"}}}}}, native: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := testSource()
			for key, value := range tc.extra {
				source[key] = value
			}
			raw, _ := json.Marshal(source)
			before := bytes.Clone(raw)
			prepared, reason := PrepareRequest(raw, "account:key:thread")
			if (reason != "") != tc.native || !bytes.Equal(raw, before) {
				t.Fatalf("reason=%q mutation=%v", reason, !bytes.Equal(raw, before))
			}
			if !tc.native && (prepared == nil || len(prepared.body) == 0) {
				t.Fatal("missing prepared request")
			}
		})
	}
}

func TestPreparedRequestFullToolHistoryNeedsNoPriorRequest(t *testing.T) {
	for _, tc := range []struct{ name, field, kind, value string }{
		{"echo", "value", "function", "quote\"\\\n中文"},
		{"python", "code", "function", "print('\\n')\n# exact code"},
		{"exec_command", "cmd", "function", "printf '%s' '\\test'\n"},
		{"patch", "input", "custom", "*** Begin Patch\n*** Add File: x\n+中文\n*** End Patch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := testSource()
			tool := object{"name": tc.name, "type": tc.kind}
			envelope := object{"name": tc.name}
			if tc.kind == "custom" {
				envelope["input"] = tc.value
			} else {
				tool["parameters"] = object{"type": "object", "properties": object{tc.field: object{"type": "string"}}}
				envelope["arguments"] = object{tc.field: tc.value}
			}
			source["tools"] = []any{tool}
			raw, _ := json.Marshal(source)
			initial, reason := PrepareRequest(raw, "account:key:thread")
			if reason != "" {
				t.Fatalf("initial request: %s", reason)
			}
			wire := sse(object{"type": "response.completed", "response": object{"id": "resp_first", "status": "completed", "output": []any{nativeCall(envelope)}}})
			stream := initial.Stream(context.Background(), io.NopCloser(strings.NewReader(wire)), nil)
			defer func() { _ = stream.Close() }()
			var call object
			if err := readEvents(stream, func(kind string, data []byte) error {
				if kind == "response.completed" {
					var event object
					if err := decode(data, &event); err != nil {
						return err
					}
					response := mustTestValue[object](t, event["response"])
					call = mustTestValue[object](t, mustTestValue[[]any](t, response["output"])[0])
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if call == nil {
				t.Fatal("tool call was not delivered")
			}
			result := object{"type": text(call["type"]) + "_output", "call_id": call["call_id"], "output": "recorded result"}
			source["input"] = []any{message("user", "hi"), call, result}
			raw, _ = json.Marshal(source)
			var previousInput string
			for _, identity := range []string{"account:key:thread", "account:key:thread", "other-account:key:thread"} {
				prepared, reason := PrepareRequest(raw, identity)
				if reason != "" {
					t.Fatalf("full history should not depend on an earlier process/account: %s", reason)
				}
				body, err := io.ReadAll(prepared.Body())
				if err != nil {
					t.Fatal(err)
				}
				var request object
				if err := decode(body, &request); err != nil {
					t.Fatal(err)
				}
				input := mustTestValue[[]any](t, request["input"])
				native := mustTestValue[object](t, input[len(input)-2])
				restored, err := prepared.bridge.translateCall(native)
				if err != nil || historyCallFingerprint(restored) != historyCallFingerprint(call) {
					t.Fatalf("full-history replay changed tool payload: %v", err)
				}
				if got := mustTestValue[object](t, input[len(input)-1]); got["output"] != result["output"] || got["call_id"] != call["call_id"] {
					t.Fatal("full-history replay changed tool result")
				}
				encoded, _ := json.Marshal(input)
				if previousInput != "" && previousInput != string(encoded) {
					t.Fatal("input changed across independent requests/accounts")
				}
				previousInput = string(encoded)
			}
			// Even after a successful response and a complete follow-up, an orphan
			// result must not be completed from another request's retained state.
			source["input"] = []any{result}
			raw, _ = json.Marshal(source)
			if prepared, reason := PrepareRequest(raw, "account:key:thread"); prepared != nil || reason != FallbackProtocol {
				t.Fatalf("orphan result inherited state: prepared=%v reason=%s", prepared != nil, reason)
			}
		})
	}
}

func TestPreparedRequestIdentityIsDeterministicWithoutRetainingBodies(t *testing.T) {
	raw, _ := json.Marshal(testSource())
	first, reason := PrepareRequest(raw, "account:key:thread")
	if reason != "" {
		t.Fatal(reason)
	}
	one, _ := io.ReadAll(first.Body())
	two, _ := io.ReadAll(first.Body())
	if !bytes.Equal(one, two) {
		t.Fatal("reading the body consumed shared request state")
	}
	for i := range one {
		one[i] = 0
	}
	again, reason := PrepareRequest(raw, "account:key:thread")
	if reason != "" {
		t.Fatal(reason)
	}
	three, _ := io.ReadAll(again.Body())
	if !bytes.Equal(two, three) {
		t.Fatal("preparation depended on a prior reader/request")
	}
}

func TestBasispointsProfileHeaders(t *testing.T) {
	headers := Headers("Bearer test", "account")
	for name, want := range map[string]string{
		"Authorization": "Bearer test", "ChatGPT-Account-ID": "account", "X-OpenAI-Account-ID": "account",
		"Origin": "https://bps.openai.com", "X-Basispoints-Auth-Mode": "chatgpt", "Accept": "text/event-stream", "Accept-Encoding": "identity",
		"X-OpenAI-Internal-Basispoints-Client-Agent-Profile": "excel",
		"X-OpenAI-Internal-Basispoints-Client-Editor":        "excel", "X-OpenAI-Internal-Basispoints-Client-Host": "office",
		"X-OpenAI-Internal-Basispoints-Client-Platform": "excel", "X-OpenAI-Internal-Basispoints-Client-Platform-Class": "PC",
		"X-OpenAI-Internal-Basispoints-Client-Product": "basispoints-excel-plugin", "X-OpenAI-Internal-Basispoints-Client-Runtime": "desktop",
		"X-OpenAI-Internal-Basispoints-Office-Host": "Excel", "X-OpenAI-Internal-Basispoints-Office-Platform": "PC",
		"X-Stainless-Lang": "js", "X-Stainless-Runtime": "browser:chrome", "User-Agent": "Mozilla/5.0",
	} {
		if got := headers.Get(name); got != want {
			t.Errorf("%s=%q want %q", name, got, want)
		}
	}
	headers.Set("Authorization", "modified")
	if Headers("Bearer test", "account").Get("Authorization") != "Bearer test" {
		t.Fatal("shared mutable profile")
	}
}

func TestModelUnavailableRequiresExplicitRejection(t *testing.T) {
	raw := []byte(`{"error":{"code":"model_not_supported"}}`)
	for _, status := range []int{200, 401, 429, 500, 502, 503} {
		if ModelUnavailable(status, raw) {
			t.Fatalf("replayed status %d", status)
		}
	}
	for _, code := range []string{"model_not_supported", "model_not_found", "model_access_denied"} {
		raw, _ := json.Marshal(object{"error": object{"code": code}})
		if !ModelUnavailable(http.StatusForbidden, raw) {
			t.Fatalf("did not recognize %s", code)
		}
	}
	if ModelUnavailable(403, []byte(`{"error":{"message":"forbidden"}}`)) {
		t.Fatal("ambiguous denial was replayed")
	}
}
