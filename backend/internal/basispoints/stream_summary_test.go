package basispoints

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStreamSummaryPreservesWireAndExplainsWithheldTools(t *testing.T) {
	const deltas = 19353
	const secret = "PRIVATE_TOOL_PAYLOAD"
	source := testSource()
	source["tools"] = []any{object{"type": "function", "name": "shell"}}
	raw, _ := json.Marshal(source)
	native := nativeCall(object{"name": "shell", "arguments": object{"cmd": secret}})
	for _, terminal := range []string{"response.completed", "response.incomplete", "response.failed", "eof", "invalid"} {
		t.Run(terminal, func(t *testing.T) {
			var wire strings.Builder
			wire.WriteString(sse(object{"type": "response.created", "response": object{"id": "resp_test", "status": "in_progress"}}))
			wire.WriteString(sse(object{"type": "response.output_text.delta", "delta": "visible"}))
			wire.WriteString(sse(object{"type": "response.reasoning_summary_text.delta", "delta": "thinking"}))
			for i := 0; i < deltas; i++ {
				wire.WriteString(sse(object{"type": "response.function_call_arguments.delta", "delta": secret}))
			}
			wire.WriteString(sse(object{"type": "response.output_item.done", "item": native}))
			if terminal == "invalid" {
				wire.WriteString("data: invalid json\n\n")
			} else if terminal != "eof" {
				wire.WriteString(sse(object{"type": terminal, "response": object{
					"id": "resp_test", "status": strings.TrimPrefix(terminal, "response."),
					"output": []any{native}, "incomplete_details": object{"reason": "max_output_tokens"},
					"usage": object{"output_tokens": 128000, "output_tokens_details": object{"reasoning_tokens": 0}},
				}}))
			}
			run := func(report func(StreamSummary)) (string, error) {
				prepared, reason := PrepareRequest(raw, "scope")
				if reason != "" {
					t.Fatal(reason)
				}
				var observed func(StreamSummary)
				reported := make(chan struct{})
				if report != nil {
					observed = func(summary StreamSummary) {
						report(summary)
						close(reported)
					}
				}
				body := prepared.Stream(context.Background(), io.NopCloser(strings.NewReader(wire.String())), observed)
				out, err := io.ReadAll(body)
				_ = body.Close()
				if report != nil {
					<-reported
				}
				return string(out), err
			}
			before, beforeErr := run(nil)
			var summary StreamSummary
			calls := 0
			after, afterErr := run(func(value StreamSummary) { summary = value; calls++ })
			if before != after || (beforeErr == nil) != (afterErr == nil) {
				t.Fatal("observation changed translation or completion")
			}
			if calls != 1 || summary.ToolDeltaEvents != deltas || summary.ToolDeltaBytes != int64(deltas*len(secret)) || summary.NativeToolsDone != 1 {
				t.Fatalf("lost upstream progress: calls=%d summary=%+v", calls, summary)
			}
			if summary.TextDeltaEvents != 1 || summary.TextDeltaBytes != 7 || summary.ReasoningDeltaEvents != 1 || summary.ReasoningDeltaBytes != 8 {
				t.Fatalf("misclassified content: %+v", summary)
			}
			if terminal == "response.completed" && summary.DeliveredTools != 1 || terminal != "response.completed" && summary.DeliveredTools != 0 {
				t.Fatalf("incorrect delivery counts: %+v", summary)
			}
			if terminal == "response.incomplete" && (summary.OutputTokens != 128000 || !summary.ReasoningTokensReported || summary.ReasoningTokens != 0 || summary.IncompleteReason != "max_output_tokens" || summary.TerminalTools != 1) {
				t.Fatalf("lost incomplete diagnostic: %+v", summary)
			}
			encoded, _ := json.Marshal(summary)
			for _, sensitive := range []string{secret, "shell", "run_officejs", "resp_test", "visible", "thinking"} {
				if strings.Contains(string(encoded), sensitive) {
					t.Fatal("summary retained content or identity")
				}
			}
			if terminal == "eof" && (!summary.ReadOrWriteError || summary.DownstreamTerminal != "") {
				t.Fatalf("EOF looked like a terminal response: %+v", summary)
			}
			if terminal == "invalid" && summary.DownstreamTerminal != "response.failed" {
				t.Fatalf("protocol failure not recorded: %+v", summary)
			}
		})
	}
}

func TestStreamSummarySlowReporterDoesNotDelayEOF(t *testing.T) {
	raw, _ := json.Marshal(testSource())
	prepared, reason := PrepareRequest(raw, "scope")
	if reason != "" {
		t.Fatal(reason)
	}
	reportStarted := make(chan struct{})
	releaseReport := make(chan struct{})
	reportFinished := make(chan struct{})
	defer func() {
		close(releaseReport)
		<-reportFinished
	}()
	body := prepared.Stream(context.Background(), io.NopCloser(strings.NewReader("")), func(StreamSummary) {
		close(reportStarted)
		<-releaseReport
		close(reportFinished)
	})
	defer func() { _ = body.Close() }()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(body)
		done <- err
	}()
	<-reportStarted
	select {
	case err := <-done:
		if err != io.ErrUnexpectedEOF {
			t.Fatalf("original stream error changed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow diagnostic callback blocked response completion")
	}
}

func TestStreamSummaryMissingReasoningUsageIsNotExplicitZero(t *testing.T) {
	var summary StreamSummary
	summary.observeUpstream("response.incomplete", object{"response": object{
		"incomplete_details": object{"reason": "PRIVATE_ERROR_MESSAGE"},
		"usage":              object{"output_tokens": json.Number("128000")},
		"output":             []any{object{"type": "function_call", "status": "in_progress", "arguments": "partial"}},
	}}, 1)
	if summary.ReasoningTokensReported || summary.IncompleteReason != "unknown" || summary.UnfinishedTools != 1 || summary.TerminalToolBytes != 7 {
		t.Fatalf("unsafe or ambiguous diagnostic: %+v", summary)
	}
}

func BenchmarkStreamSummaryToolDelta(b *testing.B) {
	payload := object{"delta": strings.Repeat("x", 1024)}
	var summary StreamSummary
	b.ReportAllocs()
	b.SetBytes(1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		summary.observeUpstream("response.function_call_arguments.delta", payload, 1100)
	}
}
