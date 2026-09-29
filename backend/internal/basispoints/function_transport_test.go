package basispoints

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func functionTransportTestTool(name, field string) object {
	return object{"type": "function", "name": name, "parameters": object{"type": "object", "required": []any{field}, "additionalProperties": false, "properties": object{
		field: object{"type": "string"}, "description": object{"type": "string"}, "title": object{"type": "string"}, "workdir": object{"type": "string"}, "shell": object{"type": "string"}, "login": object{"type": "boolean"}, "tty": object{"type": "boolean"}, "yield_time_ms": object{"type": "number"}, "max_output_tokens": object{"type": "integer"}, "timeout_ms": object{"type": "integer"}, "options": object{"type": "object"}, "prefix_rule": object{"type": "array", "items": object{"type": "string"}}, "sandbox_permissions": object{"type": "string"}, "justification": object{"type": "string"},
	}}}
}

func functionTransportTestNative(t *testing.T, name string, args object) object {
	t.Helper()
	code, err := json.Marshal(object{"name": name, "arguments": args})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(object{"summary": "Inspect requested state", "extended_summary": "Inspect the requested fixture while preserving every supplied client tool argument.", "code": string(code), "destructive": false, "references": []any{name}})
	if err != nil {
		t.Fatal(err)
	}
	return object{"type": "function_call", "name": "run_officejs", "id": "fc_fixture", "call_id": "call_fixture", "arguments": string(outer), "status": "completed"}
}

func functionTransportTestArguments(t *testing.T, call object) object {
	t.Helper()
	var args object
	if err := decode([]byte(text(call["arguments"])), &args); err != nil {
		t.Fatal(err)
	}
	return args
}

func TestFunctionTransportPreservesCompleteArguments(t *testing.T) {
	for _, field := range []string{"cmd", "code"} {
		t.Run(field, func(t *testing.T) {
			name := "exec_command"
			if field == "code" {
				name = "client.run_code"
			}
			source := testSource()
			source["tools"] = []any{functionTransportTestTool(name, field)}
			_, b := mustPrepare(t, source, "scope", nil)
			for _, code := range []string{"", "@'\r\nquotes \"中文\" literal $value C:\\fixture\r\n'@ | Write-Output\r\n", `const p = /\d+\s/; text("literal \n");`, `{"name":"another_tool","arguments":{}}`, strings.Repeat("\tprint(\"nested quote\");\n", 500)} {
				args := object{field: code, "workdir": "C:\\fixture path\\中文", "login": false, "tty": false, "yield_time_ms": json.Number("0"), "timeout_ms": json.Number("0"), "title": "", "max_output_tokens": json.Number("1234"), "prefix_rule": []any{"Write-Output"}, "sandbox_permissions": "use_default", "justification": "Fixture only", "options": object{"enabled": false, "nullable": nil, "items": []any{"", json.Number("9007199254740993"), object{"x": "a\tb"}}}}
				for _, description := range []string{"Measure coordinator reads while candidate inventory scan advances.", "", "{not JSON", `{"cmd":"must not override the payload","extra":true}`} {
					native := functionTransportTestNative(t, name, args)
					outer := transportArguments(native)
					outer["summary"], outer["extended_summary"] = description, description
					raw, _ := json.Marshal(outer)
					native["arguments"] = string(raw)
					before, _ := json.Marshal(native)
					call, err := b.translateCall(native)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(functionTransportTestArguments(t, call), args) {
						t.Fatal("arguments were dropped, coerced, or changed by a description")
					}
					after, _ := json.Marshal(native)
					if !bytes.Equal(before, after) {
						t.Fatal("native input was mutated")
					}
				}
			}
		})
	}
}

func TestFunctionTransportRejectsSplitPayloadAndInvalidArguments(t *testing.T) {
	source := testSource()
	source["tools"] = []any{functionTransportTestTool("exec_command", "cmd")}
	_, b := mustPrepare(t, source, "scope", nil)
	for _, payload := range []any{nil, "private raw command", object{"cmd": "private raw command"}, []any{}, object{"name": "exec_command", "arguments": nil}, object{"name": "exec_command", "arguments": object{}}, object{"name": "exec_command", "arguments": object{"cmd": 42}}, object{"name": "exec_command", "arguments": object{"cmd": "fixture", "undeclared": true}}} {
		native := functionTransportTestNative(t, "exec_command", object{"cmd": "fixture"})
		outer := transportArguments(native)
		if _, ok := payload.(string); ok {
			outer["code"] = payload
		} else {
			raw, _ := json.Marshal(payload)
			outer["code"] = string(raw)
		}
		outer["extended_summary"] = `{"workdir":"must not be read"}`
		raw, _ := json.Marshal(outer)
		native["arguments"] = string(raw)
		if _, err := b.translateCall(native); err == nil || strings.Contains(err.Error(), "private raw command") {
			t.Fatal("invalid function payload accepted or source leaked")
		}
	}
}

func TestFunctionTransportEnvelopeSizeLimit(t *testing.T) {
	source := testSource()
	source["tools"] = []any{functionTransportTestTool("exec_command", "cmd")}
	_, b := mustPrepare(t, source, "scope", nil)
	overhead, _ := json.Marshal(object{"name": "exec_command", "arguments": object{"cmd": ""}})
	for _, extra := range []int{0, 1} {
		native := functionTransportTestNative(t, "exec_command", object{"cmd": strings.Repeat("x", maxEnvelopeBytes-len(overhead)+extra)})
		_, err := b.translateCall(native)
		if (err != nil) != (extra == 1) {
			t.Fatal("full envelope byte limit not enforced")
		}
	}
}

func TestFunctionTransportHistoryAndCatalogUseCompleteEnvelope(t *testing.T) {
	source := testSource()
	source["tools"] = []any{object{"type": "namespace", "name": "client", "tools": []any{functionTransportTestTool("exec_command", "cmd")}}}
	wire, b := mustPrepare(t, source, "scope", new(ReplayCache))
	textWire, _ := json.Marshal(wire)
	for _, retired := range []string{"FUNCTION_CODE", "FUNCTION_CMD", "metadata JSON in extended_summary"} {
		if bytes.Contains(textWire, []byte(retired)) {
			t.Fatal("retired split transport still advertised")
		}
	}
	args := object{"cmd": "Write-Output \"literal\"\r\n", "workdir": "C:\\fixture", "login": false, "yield_time_ms": json.Number("0")}
	native := functionTransportTestNative(t, "client.exec_command", args)
	call, err := b.translateCall(native)
	if err != nil {
		t.Fatal(err)
	}
	source["input"] = []any{call, object{"type": "function_call_output", "call_id": call["call_id"], "output": "recorded"}}
	for _, cache := range []*ReplayCache{b.replay, nil, new(ReplayCache)} {
		next, replay := mustPrepare(t, source, "scope", cache)
		input := next["input"].([]any)
		restored := input[len(input)-2].(object)
		outer := transportArguments(restored)
		envelope, err := decodeTransportEnvelope(outer["code"])
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(envelope["arguments"], args) {
			t.Fatal("history lost full argument object")
		}
		again, err := replay.translateCall(restored)
		if err != nil || historyCallFingerprint(again) != historyCallFingerprint(call) {
			t.Fatalf("history changed operation: %v", err)
		}
	}
}
