package basispoints

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestOptionalRawFieldsKeepJSONEnvelopeAndAbsentValues(t *testing.T) {
	for _, field := range []string{"cmd", "code"} {
		name := "exec_command"
		decl := object{"type": "function", "name": name, "parameters": object{"type": "object", "properties": object{field: object{"type": "string"}}}}
		for _, args := range []object{{}, {field: "exact input"}} {
			encoded, _ := json.Marshal(args)
			original := object{"type": "function_call", "name": name, "call_id": "call_optional", "arguments": string(encoded)}
			source := testSource()
			source["tools"] = []any{decl}
			source["input"] = []any{original, object{"type": "function_call_output", "call_id": "call_optional", "output": "done"}}
			wire, b := mustPrepare(t, source, "optional", nil)
			if b.rawTransportField(name) != "" {
				t.Fatal("optional field enabled raw transport")
			}
			input := wire["input"].([]any)
			restored, err := b.translateCall(input[len(input)-2].(object))
			if err != nil || historyCallFingerprint(restored) != historyCallFingerprint(original) {
				t.Fatalf("optional payload changed: %v", err)
			}
		}
	}
}

// Regression for the four-call batches observed on 2026-09-29. Synthetic
// commands only; this exercises the production streaming entry without I/O.
func TestPreparedRequestRawCommandBatchUsesCatalogReferences(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		source := testSource()
		source["tools"] = []any{functionCmdTestTool("exec_command")}
		raw, _ := json.Marshal(source)
		prepared, err := PrepareRequest(raw, "batch-regression")
		if err != nil {
			t.Fatal(err)
		}
		commands := []string{"@'\r\nquoted \"中文\" C:\\fixture\r\n'@", "Get-Item -LiteralPath 'C:\\fixture'", `{"name":"not_a_tool_call","arguments":{}}`, "  literal $value; \\n\t"}
		var calls []any
		for i, code := range commands {
			call := functionCmdTestNative(t, "exec_command", code, `{"description":"Inspect fixture","yield_time_ms":10000}`)
			call["call_id"], call["id"] = "call_"+string(rune('a'+i)), "fc_"+string(rune('a'+i))
			if ambiguous && i == len(commands)-1 {
				args := transportArguments(call)
				args["references"] = []any{"exec_command", "exec_command"}
				encoded, _ := json.Marshal(args)
				call["arguments"] = string(encoded)
			}
			calls = append(calls, call)
		}
		wire := ""
		for i, call := range calls {
			wire += sse(object{"type": "response.output_item.done", "output_index": i, "item": call})
		}
		wire += sse(object{"type": "response.completed", "response": object{"id": "resp_batch", "status": "completed", "output": calls}})
		body := prepared.Stream(context.Background(), io.NopCloser(strings.NewReader(wire)))
		output, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			t.Fatal(err)
		}
		emitted, completed, failed := 0, 0, 0
		err = readEvents(bytes.NewReader(output), func(_ string, raw []byte) error {
			var event object
			if err := decode(raw, &event); err != nil {
				return err
			}
			switch event["type"] {
			case "response.output_item.done":
				call := event["item"].(object)
				args := functionCmdTestArguments(t, call)
				if emitted >= len(commands) || args["cmd"] != commands[emitted] || args["description"] != "Inspect fixture" || args["yield_time_ms"] != json.Number("10000") || call["name"] != "exec_command" {
					t.Fatal("command batch changed")
				}
				emitted++
			case "response.completed":
				completed++
			case "response.failed":
				failed++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if ambiguous {
			if emitted != 0 || completed != 0 || failed != 1 {
				t.Fatal("ambiguous late target partially dispatched the batch")
			}
		} else if emitted != len(commands) || completed != 1 || failed != 0 {
			t.Fatal("descriptive summaries failed the command batch")
		}
	}
}

func TestRawTransportIgnoresSummaryAndPreservesPayload(t *testing.T) {
	source := testSource()
	source["tools"] = []any{object{"type": "custom", "name": "functions.apply_patch"}, functionCodeTestTool("run_code"), functionCmdTestTool("exec_command")}
	_, b := mustPrepare(t, source, "scope", nil)
	for _, name := range []string{"functions.apply_patch", "run_code", "exec_command"} {
		for _, summary := range []any{nil, 42, "Read current status", "codex2api.custom/wrong_target", "codex2api.function_cmd/missing"} {
			for _, code := range []string{"", "  exact raw input \r\n\t", `{"name":"another_tool","arguments":{}}`, "```json\n{\"tool\":\"different\"}\n```", strings.Repeat("+\t中文 \"nested\" C:\\fixture\r\n", 400)} {
				args := object{"summary": summary, "references": []any{name}, "code": code, "extended_summary": `{"description":"inspect"}`}
				before, _ := json.Marshal(args)
				var wire object
				if err := decode(before, &wire); err != nil {
					t.Fatal(err)
				}
				envelope, field, err := b.toolTransportEnvelope(wire)
				if err != nil || envelope["name"] != name || field != b.rawTransportField(name) {
					t.Fatalf("summary affected catalog routing: %v", err)
				}
				value := envelope["input"]
				if field != "input" {
					value = envelope["arguments"].(object)[field]
				}
				after, _ := json.Marshal(wire)
				if value != code || string(before) != string(after) {
					t.Fatal("raw payload or native arguments changed")
				}
			}
		}
	}
}

func TestRawTransportRejectsInvalidReferencesWithoutSummaryFallback(t *testing.T) {
	b := &Bridge{tools: map[string]tool{"apply_patch": {Kind: "custom", Name: "apply_patch"}}}
	for _, refs := range []any{nil, 42, "apply_patch", []any{}, []any{""}, []any{7}, []any{"apply_patch", "apply_patch"}, []any{"apply_patch", "another"}, []any{"missing"}, []any{" apply_patch"}, []any{"apply_patch\n"}, []any{"namespace/apply_patch"}} {
		_, _, err := b.toolTransportEnvelope(object{"summary": "codex2api.custom/apply_patch", "code": "private-code-never-in-errors", "references": refs})
		if err == nil || strings.Contains(err.Error(), "private-code-never-in-errors") {
			t.Fatal("invalid reference accepted or payload leaked")
		}
	}
	if _, _, err := b.toolTransportEnvelope(object{"summary": "codex2api.custom/apply_patch", "code": "raw input"}); err == nil {
		t.Fatal("removed summary routing is still active")
	}
}

func TestRawCustomTransportValidatesCodeTypeAndSize(t *testing.T) {
	b := &Bridge{tools: map[string]tool{"apply_patch": {Kind: "custom", Name: "apply_patch"}}}
	for _, code := range []any{nil, 1, object{"input": "private"}, []any{"private"}, strings.Repeat("x", maxEnvelopeBytes+1)} {
		if _, _, err := b.toolTransportEnvelope(object{"references": []any{"apply_patch"}, "code": code}); err == nil {
			t.Fatal("invalid raw payload accepted")
		}
	}
	input := strings.Repeat("x", maxEnvelopeBytes)
	if envelope, _, err := b.toolTransportEnvelope(object{"references": []any{"apply_patch"}, "code": input}); err != nil || !reflect.DeepEqual(envelope, object{"name": "apply_patch", "input": input}) {
		t.Fatal("exact byte limit rejected")
	}
}

func TestFunctionEnvelopeReferenceMustMatchTarget(t *testing.T) {
	b := &Bridge{tools: map[string]tool{"first": {Kind: "function"}, "second": {Kind: "function"}}}
	for _, target := range []string{"first", "second"} {
		envelope, _, err := b.toolTransportEnvelope(object{"references": []any{target}, "code": `{"name":"first","arguments":{}}`})
		if target == "first" && (err != nil || envelope["name"] != "first") {
			t.Fatal("matching reference rejected")
		}
		if target == "second" && err == nil {
			t.Fatal("conflicting reference accepted")
		}
	}
}
