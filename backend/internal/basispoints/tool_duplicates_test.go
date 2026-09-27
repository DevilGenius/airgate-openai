package basispoints

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPreparedDuplicateToolsKeepCurrentDeclaration(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		for _, schemaField := range []string{"parameters", "inputSchema", "input_schema"} {
			t.Run(kind+"/"+schemaField, func(t *testing.T) {
				current := object{"type": kind, "name": "execute", "description": "Current execution instructions"}
				history := object{"type": kind, "name": "execute", "description": "Historical instructions", "defer_loading": true}
				if kind == "function" {
					schema := object{"type": "object", "properties": object{"value": object{"type": "string"}}}
					current["parameters"], history[schemaField] = schema, schema
				} else {
					format := object{"type": "grammar", "syntax": "lark", "definition": "start: /[a-z]+/"}
					current["format"], history["format"] = format, format
				}
				namespace := func(tool object) object {
					return object{"type": "namespace", "name": "functions", "tools": []any{tool}}
				}
				source := testSource()
				source["tools"] = []any{namespace(current)}
				source["input"] = []any{object{"type": "additional_tools", "tools": []any{namespace(history)}}, message("user", "continue")}
				for _, identity := range []string{"first-account", "other-account"} {
					body, prepared := prepareHistoryFixture(t, source, identity)
					info := prepared.bridge.tools["functions.execute"]
					if len(prepared.bridge.tools) != 1 || info.Catalog["description"] != current["description"] || info.Namespace != "functions" {
						t.Fatal("historical discovery annotations replaced the current contract")
					}
					wire, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(wire), "Current execution instructions") || strings.Contains(string(wire), "Historical instructions") {
						t.Fatal("wire catalog contains stale instructions")
					}
				}
			})
		}
	}
}

func TestPreparedDuplicateToolsRejectContractChanges(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second object
	}{
		{"schema", object{"parameters": object{"type": "object"}}, object{"parameters": object{"type": "array"}}},
		{"strict", object{"strict": true}, object{"strict": false}},
		{"unknown constraint", object{"execution_policy": "read"}, object{"execution_policy": "write"}},
		{"tool kind", object{}, object{"type": "custom"}},
		{"custom grammar", object{"type": "custom", "format": object{"type": "text"}}, object{"type": "custom", "format": object{"type": "grammar", "syntax": "lark", "definition": "start: /a/"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := object{"type": "function", "name": "execute"}, object{"type": "function", "name": "execute"}
			for key, value := range tc.first {
				first[key] = value
			}
			for key, value := range tc.second {
				second[key] = value
			}
			source := testSource()
			source["tools"] = []any{first}
			source["input"] = []any{object{"type": "additional_tools", "tools": []any{second}}, message("user", "continue")}
			raw, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := Prepare(raw, "account", nil); err == nil || !strings.Contains(err.Error(), "conflicting duplicate") {
				t.Fatalf("execution contract conflict was lost: %v", err)
			}
			if prepared, reason := PrepareRequest(raw, "account"); prepared != nil || reason != FallbackProtocol {
				t.Fatalf("conflicting contract used BPS: %s", reason)
			}
		})
	}
}
