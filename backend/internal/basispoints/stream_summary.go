package basispoints

import "encoding/json"

// StreamSummary distinguishes upstream generation from validated client output.
// It retains only fixed counters and allowlisted terminal names, never content,
// tool names, arguments, IDs, or credentials. The stream goroutine owns it.
// Observation reuses the decoded event: O(1) per delta, O(items) once at terminal,
// O(1) memory. No growing buffer, history rescan, timer, or per-event log is added.
type StreamSummary struct {
	UpstreamEvents          int64
	UpstreamBytes           int64
	TextDeltaEvents         int64
	TextDeltaBytes          int64
	ReasoningDeltaEvents    int64
	ReasoningDeltaBytes     int64
	ToolDeltaEvents         int64
	ToolDeltaBytes          int64
	NativeToolsDone         int64
	DeliveredTools          int64
	DownstreamEvents        int64
	UpstreamTerminal        string
	DownstreamTerminal      string
	IncompleteReason        string
	TerminalTools           int64
	UnfinishedTools         int64
	TerminalToolBytes       int64
	OutputTokens            int64
	ReasoningTokens         int64
	ReasoningTokensReported bool
	DurationMs              int64
	ReadOrWriteError        bool
}

func (s *StreamSummary) observeUpstream(kind string, payload object, size int) {
	if s == nil {
		return
	}
	s.UpstreamEvents++
	s.UpstreamBytes += int64(size)
	switch kind {
	case "response.output_text.delta":
		s.TextDeltaEvents++
		s.TextDeltaBytes += int64(len(text(payload["delta"])))
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		s.ReasoningDeltaEvents++
		s.ReasoningDeltaBytes += int64(len(text(payload["delta"])))
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		s.ToolDeltaEvents++
		s.ToolDeltaBytes += int64(len(text(payload["delta"])))
	case "response.output_item.done":
		item, _ := payload["item"].(object)
		if isTool(item) {
			s.NativeToolsDone++
		}
	case "response.completed", "response.incomplete", "response.failed", "error":
		s.UpstreamTerminal = kind
		response, _ := payload["response"].(object)
		if kind == "response.incomplete" {
			details, _ := response["incomplete_details"].(object)
			switch reason := text(details["reason"]); reason {
			case "max_output_tokens", "content_filter":
				s.IncompleteReason = reason
			default:
				s.IncompleteReason = "unknown"
			}
		}
		usage, _ := response["usage"].(object)
		s.OutputTokens, _ = summaryTokens(usage["output_tokens"])
		details, _ := usage["output_tokens_details"].(object)
		s.ReasoningTokens, s.ReasoningTokensReported = summaryTokens(details["reasoning_tokens"])
		output, _ := response["output"].([]any)
		for _, raw := range output {
			item, _ := raw.(object)
			if isTool(item) {
				s.TerminalTools++
				s.TerminalToolBytes += int64(len(text(item["arguments"])) + len(text(item["input"])))
				if text(item["status"]) != "completed" {
					s.UnfinishedTools++
				}
			}
		}
	}
}

func (s *StreamSummary) observeDownstream(kind string, payload object) {
	if s == nil {
		return
	}
	s.DownstreamEvents++
	switch kind {
	case "response.output_item.done":
		item, _ := payload["item"].(object)
		if isTool(item) {
			s.DeliveredTools++
		}
	case "response.completed", "response.incomplete", "response.failed", "error":
		s.DownstreamTerminal = kind
	}
}

func summaryTokens(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := number.Int64()
	return n, err == nil && n >= 0
}
