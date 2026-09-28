package basispoints

import (
	"strings"
	"testing"
)

func TestReferencedRawCustomCallValidatesCatalogAndIdentity(t *testing.T) {
	source := testSource()
	source["tools"] = []any{object{"type": "namespace", "name": "functions", "tools": []any{object{"type": "custom", "name": "exec"}, object{"type": "function", "name": "shell"}}}}
	_, b := mustPrepare(t, source, "scope", new(ReplayCache))
	raw := strings.Repeat("const x = {path: 'C:\\work', nested: `quote \" and ${1}`}\r\n\ttext(x);\n", 150)
	native := object{"type": "function_call", "id": "fc_raw", "call_id": "call_raw", "name": "run_officejs", "arguments": object{"summary": "Run client tool", "references": []any{"functions.exec"}, "code": raw}}
	call, err := b.translateCall(native)
	if err != nil || call["type"] != "custom_tool_call" || call["namespace"] != "functions" || call["name"] != "exec" || call["input"] != raw {
		t.Fatalf("referenced custom call did not preserve identity/input: %v", err)
	}
	args := mustTestValue[object](t, native["arguments"])
	for _, invalid := range []string{"functions.shell", "functions.missing"} {
		args["references"] = []any{invalid}
		if _, err := b.translateCall(native); err == nil {
			t.Fatal("raw mode bypassed the declared custom-tool catalog")
		}
	}
	args["references"] = []any{"functions.exec"}
	delete(native, "call_id")
	if _, err := b.translateCall(native); err == nil {
		t.Fatal("raw mode bypassed call identity validation")
	}
}
