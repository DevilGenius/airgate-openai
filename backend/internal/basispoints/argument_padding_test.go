package basispoints

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestNativePaddingAcrossChunksAndInsideCode(t *testing.T) {
	for _, size := range []int{1, 7, 256, 4096} {
		var state nativeArgumentPadding
		if err := state.consume("{\"references\":["); err != nil {
			t.Fatal(err)
		}
		padding := strings.Repeat("\t\n \r", maxNativeArgumentPadding/4)
		for i := 0; i < len(padding); i += size {
			if err := state.consume(padding[i:min(i+size, len(padding))]); err != nil {
				t.Fatalf("rejected allowed padding: %v", err)
			}
		}
		if err := state.consume(" "); err == nil {
			t.Fatal("unbounded whitespace accepted")
		}
		code := strings.Repeat(" \t\n", maxNativeArgumentPadding*4) + "\\\"\\\\end"
		wire, _ := json.Marshal(object{"code": code, "references": []any{"client.tool"}})
		state = nativeArgumentPadding{}
		for i := 0; i < len(wire); i += size {
			if err := state.consume(string(wire[i:min(i+size, len(wire))])); err != nil {
				t.Fatalf("code content rejected at chunk %d: %v", size, err)
			}
		}
	}
}

func TestNativePaddingOnlyTracksOfficeEnvelopeItems(t *testing.T) {
	var guard nativeArgumentPaddingGuard
	add := func(id, kind, name, prefix string) {
		t.Helper()
		if err := guard.observe("response.output_item.added", object{"item": object{"id": id, "type": kind, "name": name, "arguments": prefix}}); err != nil {
			t.Fatal(err)
		}
	}
	delta := func(id, value string) error {
		return guard.observe("response.function_call_arguments.delta", object{"item_id": id, "delta": value})
	}
	add("native", "function_call", "run_officejs", "{\"code\":\"")
	add("other", "function_call", "client.tool", "")
	add("custom", "custom_tool_call", "run_officejs", "")
	long := strings.Repeat(" ", maxNativeArgumentPadding+1)
	for _, id := range []string{"native", "other", "custom", "unknown"} {
		if err := delta(id, long); err != nil {
			t.Fatalf("rejected code or unrelated tool %s: %v", id, err)
		}
	}
	if err := delta("native", "\",\"references\":["); err != nil {
		t.Fatal(err)
	}
	add("second", "function_call", "functions.run_officejs", "")
	if err := delta("native", long[:maxNativeArgumentPadding]); err != nil {
		t.Fatal(err)
	}
	if err := delta("second", "{\"references\":[\"client.tool\"]}"); err != nil {
		t.Fatal(err)
	}
	if err := delta("native", " "); err == nil {
		t.Fatal("interleaved tool reset padding state")
	}
	if err := guard.observe("response.output_item.done", object{"item": object{"id": "native"}}); err != nil {
		t.Fatal(err)
	}
	if guard.items["native"] != nil {
		t.Fatal("completed item retained its state")
	}
}

func TestNativeReferencePaddingFailsBeforeUpstreamEOF(t *testing.T) {
	source := testSource()
	source["tools"] = []any{object{"type": "custom", "name": "functions.exec"}}
	_, bridge := mustPrepare(t, source, "padding", nil)
	prefix := "{\"summary\":\"codex2api.custom/functions.exec\",\"extended_summary\":\"diagnostic\",\"code\":\"text(1)\",\"destructive\":false,\"references\":["
	wire := sse(object{"type": "response.created", "response": object{"id": "resp_padding", "status": "in_progress"}}) +
		sse(object{"type": "response.output_item.added", "output_index": 0, "item": object{"type": "function_call", "id": "fc_padding", "call_id": "call_padding", "name": "run_officejs", "arguments": "", "status": "in_progress"}}) +
		sse(object{"type": "response.function_call_arguments.delta", "item_id": "fc_padding", "delta": prefix}) +
		sse(object{"type": "response.function_call_arguments.delta", "item_id": "fc_padding", "delta": strings.Repeat("\t\n \t\t\t\t\n", 1024)})
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	body := bridge.StreamWithToolRepair(ctx, reader, nil)
	defer func() { _ = body.Close() }()
	sent := make(chan error, 1)
	go func() { _, err := io.WriteString(writer, wire); sent <- err }()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "basispoints_protocol_error") || !strings.Contains(string(out), "non-content JSON whitespace limit") || strings.Contains(string(out), "response.completed") || strings.Contains(string(out), "response.custom_tool_call_input") {
		t.Fatalf("wrong failure boundary: %s", out)
	}
	if _, err := writer.Write([]byte("unread remainder")); err == nil {
		t.Fatal("upstream was not closed")
	}
	<-sent
}

func BenchmarkNativeArgumentPadding(b *testing.B) {
	state := nativeArgumentPadding{inString: true}
	delta := strings.Repeat(" ", 1024)
	b.ReportAllocs()
	b.SetBytes(int64(len(delta)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := state.consume(delta); err != nil {
			b.Fatal(err)
		}
	}
}
