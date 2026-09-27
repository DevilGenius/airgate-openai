package basispoints

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeReferencesMatchCatalogInPromptAndRebuiltHistory(t *testing.T) {
	for _, kind := range []string{"function", "custom", "code", "cmd"} {
		t.Run(kind, func(t *testing.T) {
			name := "tools.echo"
			decl := object{"type": "function", "name": name}
			call := object{"type": "function_call", "name": name, "call_id": "call_history", "arguments": "{}"}
			switch kind {
			case "custom":
				decl["type"] = "custom"
				call["type"] = "custom_tool_call"
				delete(call, "arguments")
				call["input"] = "text(1)"
			case "code", "cmd":
				if kind == "cmd" {
					name = "tools.exec_command"
					decl["name"] = name
					call["name"] = name
				}
				decl["parameters"] = object{"type": "object", "properties": object{kind: object{"type": "string"}}}
				args, _ := json.Marshal(object{kind: "line1\nline2"})
				call["arguments"] = string(args)
			}
			source := testSource()
			source["tools"] = []any{decl}
			source["input"] = []any{message("user", "hi"), call, object{"type": text(call["type"]) + "_output", "call_id": "call_history", "output": "done"}}
			body, bridge := mustPrepare(t, source, "references", nil)
			items := mustTestValue[[]any](t, body["input"])
			native := mustTestValue[object](t, items[len(items)-2])
			var outer object
			if err := decode([]byte(text(native["arguments"])), &outer); err != nil {
				t.Fatal(err)
			}
			refs := mustTestValue[[]any](t, outer["references"])
			if len(refs) != 1 || refs[0] != name {
				t.Fatalf("invalid native references: %#v", refs)
			}
			restored, err := bridge.translateCall(native)
			if err != nil || historyCallFingerprint(restored) != historyCallFingerprint(call) {
				t.Fatalf("reference metadata changed client payload: %v", err)
			}
			encoded, _ := json.Marshal(body["input"])
			protocol := string(encoded)
			if strings.Contains(protocol, "references=[]") || !strings.Contains(protocol, "at least one reference") {
				t.Fatal("prompt contradicts native references minItems")
			}
		})
	}
}
