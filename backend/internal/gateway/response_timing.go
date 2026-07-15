package gateway

import (
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// responseEventTiming 分别记录首个上游事件与首个真实输出。
// 所有回调都由单个响应读取循环串行调用，因此无需在热路径上加锁。
type responseEventTiming struct {
	start              time.Time
	firstEventMs       int64
	firstTokenMs       int64
	firstEventRecorded bool
	firstTokenRecorded bool
}

type responseTimingObserver struct {
	timing *responseEventTiming
}

func (o responseTimingObserver) OnTextDelta(string)      {}
func (o responseTimingObserver) OnReasoningDelta(string) {}
func (o responseTimingObserver) OnRateLimits(float64)    {}
func (o responseTimingObserver) OnRawEvent(kind string, data []byte) {
	if o.timing != nil {
		o.timing.observe(kind, data)
	}
}

func newResponseEventTiming(start time.Time) responseEventTiming {
	return responseEventTiming{start: start}
}

func (t *responseEventTiming) observe(eventType string, data []byte) {
	if t == nil || (eventType == "" && len(data) == 0) || t.start.IsZero() {
		return
	}
	if !t.firstEventRecorded {
		t.firstEventMs = time.Since(t.start).Milliseconds()
		t.firstEventRecorded = true
	}
	if !t.firstTokenRecorded && (isResponseOutputEvent(eventType, data) || (eventType == "" && isChatCompletionsOutput(data))) {
		t.firstTokenMs = time.Since(t.start).Milliseconds()
		t.firstTokenRecorded = true
	}
}

func isChatCompletionsOutput(data []byte) bool {
	for _, choice := range gjson.GetBytes(data, "choices").Array() {
		if choice.Get("text").String() != "" ||
			choice.Get("delta.content").String() != "" ||
			choice.Get("delta.reasoning_content").String() != "" ||
			choice.Get("delta.refusal").String() != "" ||
			choice.Get("delta.tool_calls.#").Int() > 0 {
			return true
		}
	}
	return false
}

// isResponseOutputEvent 判断事件是否已经包含客户端可消费的模型输出。
// response.created、rate_limits、内容 part 初始化等控制事件不计入 TTFT。
func isResponseOutputEvent(eventType string, data []byte) bool {
	switch eventType {
	case "response.output_text.delta",
		"response.reasoning_summary_text.delta",
		"response.reasoning_text.delta",
		"response.refusal.delta",
		"response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta",
		"response.mcp_call_arguments.delta",
		"response.code_interpreter_call_code.delta":
		return gjson.GetBytes(data, "delta").String() != ""

	case "response.output_item.added", "response.output_item.done":
		return isResponseToolCallItem(gjson.GetBytes(data, "item.type").String())

	case "response.function_call_arguments.done":
		return gjson.GetBytes(data, "arguments").Exists()
	case "response.custom_tool_call_input.done":
		return gjson.GetBytes(data, "input").Exists()
	case "response.output_text.done", "response.reasoning_summary_text.done", "response.reasoning_text.done":
		return gjson.GetBytes(data, "text").String() != ""
	case "response.refusal.done":
		return gjson.GetBytes(data, "refusal").String() != ""
	case "response.content_part.added", "response.content_part.done":
		return gjson.GetBytes(data, "part.text").String() != "" ||
			gjson.GetBytes(data, "part.refusal").String() != ""
	case "response.completed", "response.done", "response.incomplete":
		return responseEventContainsCompletedOutput(data)
	default:
		return false
	}
}

func isResponseToolCallItem(itemType string) bool {
	itemType = strings.TrimSpace(itemType)
	return itemType == "function_call" ||
		itemType == "custom_tool_call" ||
		strings.HasSuffix(itemType, "_call")
}

func responseEventContainsCompletedOutput(data []byte) bool {
	for _, item := range gjson.GetBytes(data, "response.output").Array() {
		itemType := item.Get("type").String()
		if isResponseToolCallItem(itemType) {
			return true
		}
		for _, content := range item.Get("content").Array() {
			if content.Get("text").String() != "" || content.Get("refusal").String() != "" {
				return true
			}
		}
		for _, summary := range item.Get("summary").Array() {
			if summary.Get("text").String() != "" {
				return true
			}
		}
	}
	return false
}
