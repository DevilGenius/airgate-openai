package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// ──────────────────────────────────────────────────────
// Responses API → Anthropic SSE 流式转换（轻量状态机）
// 参考 CLIProxyAPI translator/codex/claude/codex_claude_response.go
// ──────────────────────────────────────────────────────

// anthropicStreamState 轻量流式状态
type anthropicStreamState struct {
	HasToolCall           bool
	BlockIndex            int
	InputTokens           int
	OutputTokens          int
	CachedInputTokens     int
	CacheCreationTokens   int
	ReasoningOutputTokens int
	TextBlockOpen         bool // 当前是否已打开 text content block（用于容错上游跳过 content_part.added 的情况）
	ThinkingBlockOpen     bool // 当前是否已打开 thinking content block
	OpenToolBlocks        map[int]*anthropicToolBlock
	ToolBlocksByCallID    map[string]*anthropicToolBlock
	CurrentToolBlock      *anthropicToolBlock
	reverseNameMap        map[string]string // 缓存 short→original 工具名映射，避免每次事件重建
}

func applyResponsesUsageToAnthropicState(state *anthropicStreamState, usage gjson.Result) (int64, int64, int64, int64, int64) {
	inputTokens, outputTokens, cachedTokens, cacheCreationTokens, reasoningTokens := extractResponsesUsage(usage)
	if state != nil {
		state.InputTokens = int(inputTokens)
		state.OutputTokens = int(outputTokens)
		state.CachedInputTokens = int(cachedTokens)
		state.CacheCreationTokens = int(cacheCreationTokens)
		state.ReasoningOutputTokens = int(reasoningTokens)
	}
	return inputTokens, outputTokens, cachedTokens, cacheCreationTokens, reasoningTokens
}

type anthropicToolBlock struct {
	OutputIndex int
	BlockIndex  int
	CallID      string
	Name        string
	Args        string
	HadDelta    bool
	Started     bool
}

// convertResponsesEventToAnthropic 将单条 Responses API SSE 事件转换为 Anthropic SSE 事件字符串
// model: 回传给客户端的模型名（使用原始 Claude 模型名）
// 返回空字符串表示该事件不需要输出
func convertResponsesEventToAnthropic(rawLine []byte, originalRequest []byte, state *anthropicStreamState, model string) string {
	if len(rawLine) == 0 {
		return ""
	}

	// 提取 data: 行
	data, ok := extractSSEData(string(rawLine))
	if !ok || data == "" || data == "[DONE]" {
		return ""
	}

	root := gjson.Parse(data)
	typeStr := root.Get("type").String()

	switch typeStr {
	case "response.created":
		// message_start 的 usage 需要完整包含 Claude Code（2.1.x）内部 usage 累加器要求的所有字段：
		// - input_tokens / output_tokens / cache_creation_input_tokens / cache_read_input_tokens：4 个核心 token 计数
		// - cache_creation：嵌套对象 {ephemeral_5m_input_tokens, ephemeral_1h_input_tokens}
		//   Claude Code 内部 Mo$ 函数直接访问 H.cache_creation.ephemeral_1h_input_tokens（无可选链），
		//   缺失会导致累加器返回 undefined，进而让 cY4($) 里的 $.speed 崩溃。
		// - service_tier：配额档位，固定 "standard"（原生 Anthropic API 同样下发此字段）
		// 真实 token 数在 message_delta（response.completed）时下发，这里初始化为 0。
		// 关键：绝不设置 server_tool_use: null ——
		//   JS `a || b` 运算把 null 转成 undefined（null || undefined = undefined），
		//   SDK 后续访问 undefined.input_tokens 会崩溃。保持字段缺省即可，
		//   SDK 代码普遍用 $.server_tool_use?.web_search_requests ?? 0 形式读取，安全。
		template := `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard","inference_geo":"","iterations":[],"speed":"standard"},"content":[],"stop_reason":null}}`
		// 使用原始 Claude 模型名，让 Claude Code 正确识别模型能力（上下文按钮等）
		modelName := model
		if modelName == "" {
			modelName = root.Get("response.model").String()
		}
		template, _ = sjson.Set(template, "message.model", modelName)
		template, _ = sjson.Set(template, "message.id", normalizeAnthropicMessageID(root.Get("response.id").String()))
		// Claude 官方流式序列：message_start 之后紧跟一个 ping 事件，客户端用它确认连接已建立。
		return "event: message_start\n" + fmt.Sprintf("data: %s\n\n", template) +
			"event: ping\ndata: {\"type\":\"ping\"}\n\n"

	case "keepalive":
		// ChatGPT/Codex 上游会在长时间无输出时发 keepalive。Anthropic messages
		// 流式协议对应的是 ping 事件，透传可避免客户端长时间无事件导致读超时或 UI 假死。
		return "event: ping\ndata: {\"type\":\"ping\"}\n\n"

	case "response.reasoning_summary_part.added":
		// 若仍有未关闭的 text block，先关闭它
		closePrefix := closeOpenTextBlock(state)
		closePrefix += closeOpenToolBlocks(state)
		template := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		state.ThinkingBlockOpen = true
		return closePrefix + "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.reasoning_summary_text.delta":
		// 容错：上游若跳过 reasoning_summary_part.added，这里按需补开
		var prefix string
		if !state.ThinkingBlockOpen {
			prefix = closeOpenTextBlock(state)
			prefix += closeOpenToolBlocks(state)
			startTpl := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
			startTpl, _ = sjson.Set(startTpl, "index", state.BlockIndex)
			prefix += "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", startTpl)
			state.ThinkingBlockOpen = true
		}
		template := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		template, _ = sjson.Set(template, "delta.thinking", root.Get("delta").String())
		return prefix + "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.reasoning_summary_part.done":
		if !state.ThinkingBlockOpen {
			return ""
		}
		template := `{"type":"content_block_stop","index":0}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		state.BlockIndex++
		state.ThinkingBlockOpen = false
		return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.content_part.added":
		// 若仍有未关闭的 thinking block，先关闭
		closePrefix := closeOpenThinkingBlock(state)
		closePrefix += closeOpenToolBlocks(state)
		template := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		state.TextBlockOpen = true
		return closePrefix + "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.output_text.delta":
		// 容错：上游若跳过 content_part.added，这里按需补开 text block
		var prefix string
		if !state.TextBlockOpen {
			prefix = closeOpenThinkingBlock(state)
			prefix += closeOpenToolBlocks(state)
			startTpl := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
			startTpl, _ = sjson.Set(startTpl, "index", state.BlockIndex)
			prefix += "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", startTpl)
			state.TextBlockOpen = true
		}
		template := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		template, _ = sjson.Set(template, "delta.text", root.Get("delta").String())
		return prefix + "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.refusal.delta":
		delta := root.Get("delta").String()
		if delta == "" {
			return ""
		}
		var prefix string
		if !state.TextBlockOpen {
			prefix = closeOpenThinkingBlock(state)
			prefix += closeOpenToolBlocks(state)
			startTpl := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
			startTpl, _ = sjson.Set(startTpl, "index", state.BlockIndex)
			prefix += "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", startTpl)
			state.TextBlockOpen = true
		}
		template := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		template, _ = sjson.Set(template, "delta.text", delta)
		return prefix + "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.content_part.done":
		if !state.TextBlockOpen {
			return ""
		}
		template := `{"type":"content_block_stop","index":0}`
		template, _ = sjson.Set(template, "index", state.BlockIndex)
		state.BlockIndex++
		state.TextBlockOpen = false
		return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)

	case "response.output_item.added":
		item := root.Get("item")
		itemType := item.Get("type").String()
		if itemType == "function_call" {
			state.HasToolCall = true

			// 还原工具短名（懒初始化缓存）
			if state.reverseNameMap == nil {
				state.reverseNameMap = buildReverseToolNameMap(originalRequest)
			}
			name := item.Get("name").String()
			if orig, ok := state.reverseNameMap[name]; ok {
				name = orig
			}

			outputIndex := int(root.Get("output_index").Int())
			if state.OpenToolBlocks == nil {
				state.OpenToolBlocks = map[int]*anthropicToolBlock{}
			}
			if state.ToolBlocksByCallID == nil {
				state.ToolBlocksByCallID = map[string]*anthropicToolBlock{}
			}
			block := &anthropicToolBlock{
				OutputIndex: outputIndex,
				CallID:      item.Get("call_id").String(),
				Name:        name,
			}
			state.OpenToolBlocks[outputIndex] = block
			if block.CallID != "" {
				state.ToolBlocksByCallID[block.CallID] = block
			}

			return ""
		}
		// web_search_call 等原生工具：忽略
		return ""

	case "response.function_call_arguments.delta":
		delta := root.Get("delta").String()
		if delta == "" {
			return ""
		}
		block := lookupToolBlock(state, root)
		if block == nil {
			return ""
		}
		block.Args += delta
		block.HadDelta = true
		return startAnthropicToolBlock(state, block) + emitToolArgumentsDelta(block.BlockIndex, delta)

	case "response.function_call_arguments.done":
		block := lookupToolBlock(state, root)
		if block == nil {
			return ""
		}
		args := root.Get("arguments").String()
		if args == "" {
			args = block.Args
		}
		if args != "" && !block.HadDelta {
			block.HadDelta = true
			return startAnthropicToolBlock(state, block) + emitToolArgumentsDelta(block.BlockIndex, args)
		}
		return ""

	case "response.output_item.done":
		itemType := root.Get("item.type").String()
		if itemType == "function_call" {
			block := lookupToolBlock(state, root)
			if block == nil {
				return ""
			}
			output := startAnthropicToolBlock(state, block)
			if args := root.Get("item.arguments").String(); args != "" && !block.HadDelta {
				block.HadDelta = true
				output += emitToolArgumentsDelta(block.BlockIndex, args)
			}
			output += closeAnthropicToolBlock(state, block)
			return output
		}
		return ""

	case "response.completed", "response.done":
		// 提取 usage
		inputTokens, outputTokens, cachedTokens, cacheCreationTokens, _ := applyResponsesUsageToAnthropicState(state, root.Get("response.usage"))

		// 先关闭任何未显式关闭的 text/thinking 内容块，避免 SSE 事件序列不成对
		prefix := closeOpenTextBlock(state)
		prefix += closeOpenThinkingBlock(state)
		prefix += closeOpenToolBlocks(state)

		// 构建 message_delta
		// usage 包含 SDK accumulator 合并时需要的完整字段集（与 message_start 对齐）：
		// - input_tokens / output_tokens / cache_creation_input_tokens / cache_read_input_tokens
		// - cache_creation 嵌套对象（Claude Code 内部 usage merger 要求）
		// - service_tier（原生 Anthropic 下发）
		// 关键：绝不设置 server_tool_use: null —— 会触发 SDK `||` 短路转 undefined → 崩溃链。
		// delta.container / context_management：Claude Code 反序列化对这两字段直接读（非可选链），
		//   case "message_delta": q.container=$.delta.container, q.context_management=$.context_management
		//   缺失会让 context_management 逻辑拿到 undefined 而不是 null。
		template := `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null,"container":null},"context_management":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard","inference_geo":"","iterations":[],"speed":"standard"}}`

		var finalStop string
		if state.HasToolCall {
			finalStop = "tool_use"
		} else {
			finalStop = normalizeAnthropicStopReason(root.Get("response.stop_reason").String())
		}
		// 最终再过一层白名单校验，只允许 Anthropic 官方合法枚举
		finalStop = ensureAnthropicStopReason(finalStop)
		template, _ = sjson.Set(template, "delta.stop_reason", finalStop)

		// stop_sequence 若上游带了则透传，不会破坏合法性（null 亦合规）
		if seq := root.Get("response.stop_sequence"); seq.Exists() && seq.Type != gjson.Null {
			template, _ = sjson.SetRaw(template, "delta.stop_sequence", seq.Raw)
		}

		// refusal 场景补 stop_details：Claude Code 的 WL7 函数会读 stop_details.explanation
		// 在 UI 展示给用户。缺失时用户不知道为何被拒绝。
		if finalStop == "refusal" {
			explanation := root.Get("response.error.message").String()
			if explanation == "" {
				explanation = root.Get("response.refusal").String()
			}
			if explanation == "" {
				explanation = "content blocked by upstream policy"
			}
			stopDetails, _ := sjson.Set(`{"type":"refusal","explanation":""}`, "explanation", explanation)
			template, _ = sjson.SetRaw(template, "delta.stop_details", stopDetails)
		}

		template, _ = sjson.Set(template, "usage.input_tokens", inputTokens)
		template, _ = sjson.Set(template, "usage.output_tokens", outputTokens)
		template, _ = sjson.Set(template, "usage.cache_read_input_tokens", cachedTokens)
		template, _ = sjson.Set(template, "usage.cache_creation_input_tokens", cacheCreationTokens)

		// 把上游真实 service_tier 写回 usage，合法枚举：standard / priority / batch
		if tier := normalizeOpenAIServiceTier(root.Get("response.service_tier").String()); tier != "" {
			template, _ = sjson.Set(template, "usage.service_tier", tier)
		}

		output := prefix + "event: message_delta\n" + fmt.Sprintf("data: %s\n\n", template)
		output += "event: message_stop\n" + "data: {\"type\":\"message_stop\"}\n\n"
		return output

	case "response.failed":
		applyResponsesUsageToAnthropicState(state, root.Get("response.usage"))
		errMsg := root.Get("response.error.message").String()
		if errMsg == "" {
			errMsg = "upstream response failed"
		}
		errType := mapResponsesErrorType(root.Get("response.error.type").String(), root.Get("response.error.code").String())
		prefix := closeOpenTextBlock(state)
		prefix += closeOpenThinkingBlock(state)
		prefix += closeOpenToolBlocks(state)
		return prefix + buildAnthropicStreamError(errType, errMsg)

	case "response.incomplete":
		inputTokens, outputTokens, cachedTokens, cacheCreationTokens, _ := applyResponsesUsageToAnthropicState(state, root.Get("response.usage"))
		reason := root.Get("response.incomplete_details.reason").String()
		if reason == "" {
			reason = "unknown"
		}
		prefix := closeOpenTextBlock(state)
		prefix += closeOpenThinkingBlock(state)
		prefix += closeOpenToolBlocks(state)
		if reason == "max_output_tokens" {
			template := `{"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null,"container":null},"context_management":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard","inference_geo":"","iterations":[],"speed":"standard"}}`
			template, _ = sjson.Set(template, "usage.input_tokens", inputTokens)
			template, _ = sjson.Set(template, "usage.output_tokens", outputTokens)
			template, _ = sjson.Set(template, "usage.cache_read_input_tokens", cachedTokens)
			template, _ = sjson.Set(template, "usage.cache_creation_input_tokens", cacheCreationTokens)
			if tier := normalizeOpenAIServiceTier(root.Get("response.service_tier").String()); tier != "" {
				template, _ = sjson.Set(template, "usage.service_tier", tier)
			}
			return prefix + "event: message_delta\n" + fmt.Sprintf("data: %s\n\n", template) +
				"event: message_stop\n" + "data: {\"type\":\"message_stop\"}\n\n"
		}
		return prefix + buildAnthropicStreamError("api_error", "response incomplete: "+reason)
	}

	// 忽略未知事件（web_search_call.* 等）
	return ""
}

// closeOpenTextBlock 如果当前有未关闭的 text content block，返回对应的 content_block_stop SSE 片段；否则返回空
func closeOpenTextBlock(state *anthropicStreamState) string {
	if !state.TextBlockOpen {
		return ""
	}
	template := `{"type":"content_block_stop","index":0}`
	template, _ = sjson.Set(template, "index", state.BlockIndex)
	state.BlockIndex++
	state.TextBlockOpen = false
	return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)
}

// closeOpenThinkingBlock 如果当前有未关闭的 thinking content block，返回对应的 content_block_stop SSE 片段；否则返回空
func closeOpenThinkingBlock(state *anthropicStreamState) string {
	if !state.ThinkingBlockOpen {
		return ""
	}
	template := `{"type":"content_block_stop","index":0}`
	template, _ = sjson.Set(template, "index", state.BlockIndex)
	state.BlockIndex++
	state.ThinkingBlockOpen = false
	return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)
}

func closeOpenAnthropicContentBlocks(state *anthropicStreamState) string {
	prefix := closeOpenTextBlock(state)
	prefix += closeOpenThinkingBlock(state)
	prefix += closeOpenToolBlocks(state)
	return prefix
}

func emitToolArgumentsDelta(blockIndex int, args string) string {
	template := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`
	template, _ = sjson.Set(template, "index", blockIndex)
	template, _ = sjson.Set(template, "delta.partial_json", args)
	return "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n", template)
}

func lookupToolBlock(state *anthropicStreamState, root gjson.Result) *anthropicToolBlock {
	if state == nil {
		return nil
	}
	if outputIndex := root.Get("output_index"); outputIndex.Exists() {
		if block := state.OpenToolBlocks[int(outputIndex.Int())]; block != nil {
			return block
		}
	}
	callID := root.Get("call_id").String()
	if callID == "" {
		callID = root.Get("item.call_id").String()
	}
	if callID != "" {
		return state.ToolBlocksByCallID[callID]
	}
	return nil
}

func startAnthropicToolBlock(state *anthropicStreamState, block *anthropicToolBlock) string {
	if block == nil {
		return ""
	}
	prefix := closeOpenTextBlock(state)
	prefix += closeOpenThinkingBlock(state)
	if state.CurrentToolBlock != nil && state.CurrentToolBlock != block {
		prefix += closeAnthropicToolBlock(state, state.CurrentToolBlock)
	}
	if block.Started {
		state.CurrentToolBlock = block
		return prefix
	}
	block.BlockIndex = state.BlockIndex
	state.BlockIndex++
	block.Started = true
	state.CurrentToolBlock = block
	template := `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`
	template, _ = sjson.Set(template, "index", block.BlockIndex)
	template, _ = sjson.Set(template, "content_block.id", block.CallID)
	template, _ = sjson.Set(template, "content_block.name", block.Name)
	return prefix + "event: content_block_start\n" + fmt.Sprintf("data: %s\n\n", template)
}

func closeAnthropicToolBlock(state *anthropicStreamState, block *anthropicToolBlock) string {
	if block == nil {
		return ""
	}
	delete(state.OpenToolBlocks, block.OutputIndex)
	if block.CallID != "" {
		delete(state.ToolBlocksByCallID, block.CallID)
	}
	if state.CurrentToolBlock == block {
		state.CurrentToolBlock = nil
	}
	if !block.Started {
		return ""
	}
	block.Started = false
	template := `{"type":"content_block_stop","index":0}`
	template, _ = sjson.Set(template, "index", block.BlockIndex)
	return "event: content_block_stop\n" + fmt.Sprintf("data: %s\n\n", template)
}

func closeOpenToolBlocks(state *anthropicStreamState) string {
	if len(state.OpenToolBlocks) == 0 {
		return ""
	}
	outputIndexes := make([]int, 0, len(state.OpenToolBlocks))
	for outputIndex := range state.OpenToolBlocks {
		outputIndexes = append(outputIndexes, outputIndex)
	}
	for i := 1; i < len(outputIndexes); i++ {
		for j := i; j > 0 && state.OpenToolBlocks[outputIndexes[j-1]].BlockIndex > state.OpenToolBlocks[outputIndexes[j]].BlockIndex; j-- {
			outputIndexes[j-1], outputIndexes[j] = outputIndexes[j], outputIndexes[j-1]
		}
	}
	var out strings.Builder
	for _, outputIndex := range outputIndexes {
		out.WriteString(closeAnthropicToolBlock(state, state.OpenToolBlocks[outputIndex]))
	}
	return out.String()
}

type continuationAnchorReplayError struct {
	failure    *responsesFailureError
	replaySafe bool
}

func (e *continuationAnchorReplayError) Error() string {
	if e == nil || e.failure == nil {
		return ""
	}
	return e.failure.Error()
}

func (e *continuationAnchorReplayError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.failure
}

func canReplayContinuationAnchor(err error) bool {
	var replayErr *continuationAnchorReplayError
	if errors.As(err, &replayErr) {
		return replayErr.replaySafe
	}
	var failure *responsesFailureError
	return errors.As(err, &failure) && failure.isContinuationAnchorError()
}

func continuationAnchorReplayErr(failure *responsesFailureError, outputWritten bool) error {
	if failure != nil && failure.isContinuationAnchorError() {
		return &continuationAnchorReplayError{failure: failure, replaySafe: !outputWritten}
	}
	return nil
}

// normalizeAnthropicMessageID 把 OpenAI Responses API 的 `resp_...` id 规范化为 Anthropic 风格的 `msg_...`。
// Anthropic 官方 message id 固定使用 `msg_` 前缀，部分 SDK / 下游消费方会以此为前缀做类型识别。
// 保持后缀不变，确保和 Core 侧的请求追踪 ID 仍能对应。
func normalizeAnthropicMessageID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	if strings.HasPrefix(id, "resp_") {
		return "msg_" + strings.TrimPrefix(id, "resp_")
	}
	// 未知前缀兜底：直接加 msg_ 前缀，避免返回空或破坏下游解析
	return "msg_" + id
}

// buildAnthropicStreamError 构建 Anthropic SSE 错误事件
// errType: Anthropic 错误类型（invalid_request_error, rate_limit_error, api_error 等）
func buildAnthropicStreamError(errType, message string) string {
	return buildAnthropicStreamErrorWithCode(errType, "", message)
}

func buildAnthropicStreamErrorWithCode(errType, code, message string) string {
	if errType == "" {
		errType = "api_error"
	}
	template := `{"type":"error","error":{"type":"","message":""}}`
	template, _ = sjson.Set(template, "error.type", errType)
	template, _ = sjson.Set(template, "error.message", message)
	if code != "" {
		template, _ = sjson.Set(template, "error.code", code)
	}
	return "event: error\n" + fmt.Sprintf("data: %s\n\n", template)
}

// mapResponsesErrorType 将 Responses API 错误类型映射为 Anthropic 错误类型
// Anthropic 合法枚举：api_error / authentication_error / invalid_request_error /
//
//	not_found_error / overloaded_error / permission_error / rate_limit_error
//
// Claude Code 的重试分类器依赖这个 type 区分瞬时过载（可重试）与永久失败（不可重试）。
func mapResponsesErrorType(errType, errCode string) string {
	errType = strings.ToLower(strings.TrimSpace(errType))
	errCode = strings.ToLower(strings.TrimSpace(errCode))

	switch errType {
	case "invalid_request_error":
		return "invalid_request_error"
	case "rate_limit_error":
		return "rate_limit_error"
	case "authentication_error":
		return "authentication_error"
	case "not_found_error":
		return "not_found_error"
	case "overloaded_error":
		return "overloaded_error"
	case "permission_error":
		return "permission_error"
	// OpenAI 常见错误类型映射到最贴切的 Anthropic 枚举
	case "server_error", "internal_error":
		return "overloaded_error"
	case "insufficient_quota", "billing_not_active":
		return "permission_error"
	case "permission_denied":
		return "permission_error"
	}

	// 通过 code 推断类型
	switch errCode {
	case "context_length_exceeded", "max_tokens_exceeded", "input_too_long":
		return "invalid_request_error"
	case "rate_limit_exceeded":
		return "rate_limit_error"
	case "overloaded", "service_overloaded", "model_overloaded", "engine_overloaded":
		return "overloaded_error"
	case "insufficient_quota", "quota_exceeded", "billing_hard_limit_reached":
		return "permission_error"
	}

	return "api_error"
}

// ──────────────────────────────────────────────────────
// 非流式：Responses completed → Anthropic JSON
// ──────────────────────────────────────────────────────

// convertResponsesCompletedToAnthropicJSON 将 Responses completed 事件转为 Anthropic 非流式 JSON 响应
//
// 上游行为说明：
//   - 有的上游（如官方 Responses API）会在 response.completed 事件中带上完整的
//     response.output[] 数组，包含所有 message/reasoning/function_call 内容。
//   - 有的上游（如 ChatGPT WebSocket 会话）只把 output_text/reasoning 通过 delta 事件流式下发，
//     而 response.completed 只带 metadata（id / model / usage），output[] 缺失或为空。
//
// 因此除了尝试从 completed 事件解析 output[] 之外，还必须回退到 ParseSSEStream 聚合出的
// wsResult.Text / wsResult.Reasoning / wsResult.ToolUses，才能保证 Anthropic 客户端
// 拿到非空的 content 数组。
func convertResponsesCompletedToAnthropicJSON(
	completedJSON, originalRequest []byte,
	model string,
	wsResult *WSResult,
) string {
	root := gjson.ParseBytes(completedJSON)
	typeStr := root.Get("type").String()
	if typeStr != "response.completed" && typeStr != "response.done" {
		if typeStr != "response.incomplete" || root.Get("response.incomplete_details.reason").String() != "max_output_tokens" {
			return ""
		}
	}

	maxOutputIncomplete := typeStr == "response.incomplete"

	responseData := root.Get("response")
	if !responseData.Exists() {
		return ""
	}

	revNames := buildReverseToolNameMap(originalRequest)

	// usage 包含 Claude Code 内部 usage 累加器期望的所有字段，但不含 server_tool_use: null。
	// 详见 message_start 分支的注释：cache_creation 嵌套对象必填，server_tool_use 必须缺省。
	// container / context_management：Claude Code 反序列化对这两字段直接读（非可选链），
	// 缺失会让 context_management 逻辑拿到 undefined 而不是 null。
	out := `{"id":"","type":"message","role":"assistant","model":"","container":null,"context_management":null,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard","inference_geo":"","iterations":[],"speed":"standard"}}`
	out, _ = sjson.Set(out, "id", normalizeAnthropicMessageID(responseData.Get("id").String()))
	// 始终使用原始 Claude 模型名，让 Claude Code 正确识别模型能力
	out, _ = sjson.Set(out, "model", model)

	inputTokens, outputTokens, cachedTokens, cacheCreationTokens, _ := extractResponsesUsage(responseData.Get("usage"))
	out, _ = sjson.Set(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.Set(out, "usage.output_tokens", outputTokens)
	out, _ = sjson.Set(out, "usage.cache_read_input_tokens", cachedTokens)
	out, _ = sjson.Set(out, "usage.cache_creation_input_tokens", cacheCreationTokens)

	// 把上游真实 service_tier 写回 usage，合法枚举：standard / priority / batch
	if tier := normalizeOpenAIServiceTier(responseData.Get("service_tier").String()); tier != "" {
		out, _ = sjson.Set(out, "usage.service_tier", tier)
	}

	hasThinking := false
	hasText := false
	hasToolCall := false

	if output := responseData.Get("output"); output.Exists() && output.IsArray() {
		for _, item := range output.Array() {
			switch item.Get("type").String() {
			case "reasoning":
				thinking := collectReasoningText(item)
				if thinking != "" {
					block := `{"type":"thinking","thinking":""}`
					block, _ = sjson.Set(block, "thinking", thinking)
					out, _ = sjson.SetRaw(out, "content.-1", block)
					hasThinking = true
				}
			case "message":
				content := item.Get("content")
				if content.IsArray() {
					for _, part := range content.Array() {
						if part.Get("type").String() == "output_text" {
							if text := part.Get("text").String(); text != "" {
								block := `{"type":"text","text":""}`
								block, _ = sjson.Set(block, "text", text)
								out, _ = sjson.SetRaw(out, "content.-1", block)
								hasText = true
							}
						}
					}
				} else if text := content.String(); text != "" {
					block := `{"type":"text","text":""}`
					block, _ = sjson.Set(block, "text", text)
					out, _ = sjson.SetRaw(out, "content.-1", block)
					hasText = true
				}
			case "function_call":
				hasToolCall = true
				name := item.Get("name").String()
				if original, ok := revNames[name]; ok {
					name = original
				}
				toolBlock := `{"type":"tool_use","id":"","name":"","input":{}}`
				toolBlock, _ = sjson.Set(toolBlock, "id", item.Get("call_id").String())
				toolBlock, _ = sjson.Set(toolBlock, "name", name)
				inputRaw := "{}"
				if argsStr := item.Get("arguments").String(); argsStr != "" && gjson.Valid(argsStr) {
					argsJSON := gjson.Parse(argsStr)
					if argsJSON.IsObject() {
						inputRaw = argsJSON.Raw
					}
				}
				toolBlock, _ = sjson.SetRaw(toolBlock, "input", inputRaw)
				out, _ = sjson.SetRaw(out, "content.-1", toolBlock)
			}
		}
	}

	// 回退：completed 事件没带完整 output 时，用 ParseSSEStream 聚合的 delta 内容补齐
	if wsResult != nil {
		if !hasThinking && wsResult.Reasoning != "" {
			block := `{"type":"thinking","thinking":""}`
			block, _ = sjson.Set(block, "thinking", wsResult.Reasoning)
			out, _ = sjson.SetRaw(out, "content.-1", block)
			hasThinking = true
		}
		if !hasText && wsResult.Text != "" {
			block := `{"type":"text","text":""}`
			block, _ = sjson.Set(block, "text", wsResult.Text)
			out, _ = sjson.SetRaw(out, "content.-1", block)
			hasText = true
		}
		if !hasToolCall && len(wsResult.ToolUses) > 0 {
			for _, tu := range wsResult.ToolUses {
				name := ""
				if tu.Name != nil {
					name = *tu.Name
				}
				if original, ok := revNames[name]; ok {
					name = original
				}
				toolBlock := `{"type":"tool_use","id":"","name":"","input":{}}`
				toolBlock, _ = sjson.Set(toolBlock, "id", tu.ID)
				toolBlock, _ = sjson.Set(toolBlock, "name", name)
				inputRaw := "{}"
				if len(tu.Input) > 0 && gjson.ValidBytes(tu.Input) {
					if parsed := gjson.ParseBytes(tu.Input); parsed.IsObject() {
						inputRaw = parsed.Raw
					}
				}
				toolBlock, _ = sjson.SetRaw(toolBlock, "input", inputRaw)
				out, _ = sjson.SetRaw(out, "content.-1", toolBlock)
				hasToolCall = true
			}
		}
	}

	// 如果最终还是没有任何内容块，至少塞一个空 text block，避免客户端 SDK 访问 content[0] 崩溃
	if !hasThinking && !hasText && !hasToolCall {
		out, _ = sjson.SetRaw(out, "content.-1", `{"type":"text","text":""}`)
	}

	var finalStop string
	if hasToolCall {
		finalStop = "tool_use"
	} else if maxOutputIncomplete {
		finalStop = "max_tokens"
	} else {
		finalStop = normalizeAnthropicStopReason(responseData.Get("stop_reason").String())
	}
	finalStop = ensureAnthropicStopReason(finalStop)
	out, _ = sjson.Set(out, "stop_reason", finalStop)

	if stopSeq := responseData.Get("stop_sequence"); stopSeq.Exists() && stopSeq.Type != gjson.Null {
		out, _ = sjson.SetRaw(out, "stop_sequence", stopSeq.Raw)
	}

	// refusal 场景补 stop_details（同流式路径）
	if finalStop == "refusal" {
		explanation := responseData.Get("error.message").String()
		if explanation == "" {
			explanation = responseData.Get("refusal").String()
		}
		if explanation == "" {
			explanation = "content blocked by upstream policy"
		}
		stopDetails, _ := sjson.Set(`{"type":"refusal","explanation":""}`, "explanation", explanation)
		out, _ = sjson.SetRaw(out, "stop_details", stopDetails)
	}

	return out
}

// collectReasoningText 从 reasoning output item 中收集思考文本
func collectReasoningText(item gjson.Result) string {
	var b strings.Builder
	if summary := item.Get("summary"); summary.Exists() {
		if summary.IsArray() {
			for _, part := range summary.Array() {
				if txt := part.Get("text"); txt.Exists() {
					b.WriteString(txt.String())
				} else {
					b.WriteString(part.String())
				}
			}
		} else {
			b.WriteString(summary.String())
		}
	}
	if b.Len() == 0 {
		if content := item.Get("content"); content.Exists() {
			if content.IsArray() {
				for _, part := range content.Array() {
					if txt := part.Get("text"); txt.Exists() {
						b.WriteString(txt.String())
					} else {
						b.WriteString(part.String())
					}
				}
			} else {
				b.WriteString(content.String())
			}
		}
	}
	return b.String()
}

// ──────────────────────────────────────────────────────
// SSE 流转换入口
// ──────────────────────────────────────────────────────

// translateResponsesSSEToAnthropicSSE 读取上游 Responses API SSE 并翻译为 Anthropic SSE 写回客户端
// model: 原始 Claude 模型名（写入客户端响应体）
// mappedModel: 映射后的 GPT 模型名（写入 result.Model 供 Core 计费）
func translateResponsesSSEToAnthropicSSE(
	ctx context.Context,
	resp *http.Response,
	w http.ResponseWriter,
	model string,
	mappedModel string,
	originalRequest []byte,
	requestServiceTier string,
	defaultServiceTier string,
	start time.Time,
	compactEventIdleTimeout time.Duration,
	session openAISessionResolution,
) (sdk.ForwardOutcome, error) {
	setAnthropicStyleResponseHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	state := &anthropicStreamState{}
	// billingModel 用于 Core 计费，优先使用映射后的 GPT 模型名
	billingModel := mappedModel
	compactSummaryRequest := isAnthropicCompactSummaryRequest(gjson.ParseBytes(originalRequest))
	var compactIdleTimedOut atomic.Bool
	var compactIdleTimer *time.Timer
	if compactSummaryRequest && compactEventIdleTimeout > 0 {
		compactIdleTimer = time.AfterFunc(compactEventIdleTimeout, func() {
			compactIdleTimedOut.Store(true)
			slog.Warn("anthropic_compact_sse_idle_timeout",
				"timeout_ms", compactEventIdleTimeout.Milliseconds(),
				"model", model,
				"mapped_model", mappedModel,
				"session", session.SessionKey,
			)
			_ = resp.Body.Close()
		})
		defer compactIdleTimer.Stop()
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), upstreamSSEMaxLineBytes)

	var streamErr error
	timing := newResponseEventTiming(start)
	serviceTier := firstNonEmptyTier(requestServiceTier)
	skipCurrentOutput := false
	outputWritten := false
	responseID := ""
	terminalEventReceived := false

	for scanner.Scan() {
		skipCurrentOutput = false
		select {
		case <-ctx.Done():
			streamErr = ctx.Err()
			goto done
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// 记录结构性事件
		if data, ok := extractSSEData(string(line)); ok && data != "" && data != "[DONE]" {
			if compactIdleTimer != nil && !compactIdleTimedOut.Load() {
				compactIdleTimer.Reset(compactEventIdleTimeout)
			}
			eventType := streamDiagnosticEventType(data)
			timing.observe(eventType, []byte(data))
			if eventType != "response.output_text.delta" &&
				eventType != "response.reasoning_summary_text.delta" &&
				eventType != "response.function_call_arguments.delta" {
				slog.Debug("[上游SSE]", "type", eventType, "data", truncate(data, 300))
			}
			// 大事件诊断：上游单行 SSE 超阈值时打印 type 与长度，便于追踪触发 gRPC 上限的源头。
			if len(line) >= largeSSEEventThreshold {
				slog.Warn("[上游SSE 大事件]",
					"type", eventType,
					"line_bytes", len(line),
					"response_id", gjson.Get(data, "response.id").String(),
				)
			}

			// 捕获上游实际模型名（用于计费）
			if rm := gjson.Get(data, "response.model").String(); rm != "" {
				billingModel = rm
			}
			eventResponseID := strings.TrimSpace(gjson.Get(data, "response.id").String())
			if eventResponseID != "" {
				responseID = eventResponseID
			}
			// 终止事件集中分发，避免为 safety 单独扫描或判断正常 delta。
			switch eventType {
			case "response.completed", "response.done":
				terminalEventReceived = true
				if session.SessionKey != "" && eventResponseID != "" {
					updateSessionStateResponseID(session.SessionKey, eventResponseID, session.AccountID)
				}
				if serviceTier == "" {
					serviceTier = firstNonEmptyTier(gjson.Get(data, "response.service_tier").String(), defaultServiceTier)
				}
				usageNode := gjson.Get(data, "response.usage")
				slog.Debug("[Anthropic←Responses] 上游 usage",
					"session", session.SessionKey,
					"response_id", gjson.Get(data, "response.id").String(),
					"usage_raw", usageNode.Raw,
					"input_tokens", usageNode.Get("input_tokens").Int(),
					"cached_tokens", usageNode.Get("input_tokens_details.cached_tokens").Int(),
					"output_tokens", usageNode.Get("output_tokens").Int(),
					"response_model", gjson.Get(data, "response.model").String(),
				)

			case "response.failed":
				applyResponsesUsageToAnthropicState(state, gjson.Get(data, "response.usage"))
				terminalEventReceived = true
				streamErr = parseResponsesFailureEvent(eventType, []byte(data))
				var failure *responsesFailureError
				if errors.As(streamErr, &failure) {
					skipCurrentOutput = failure.isContinuationAnchorError() && !outputWritten
				} else if streamErr == nil {
					errMsg := gjson.Get(data, "response.error.message").String()
					if errMsg == "" {
						errMsg = "上游返回 response.failed"
					}
					streamErr = fmt.Errorf("上游错误: %s", errMsg)
				}

			case "error":
				streamErr = parseResponsesFailureEvent(eventType, []byte(data))
				if streamErr == nil {
					streamErr = fmt.Errorf("上游错误: %s", data)
				}
				// Anthropic 转换器没有原生 error 事件映射，由 done 分支统一补写错误帧。
				skipCurrentOutput = true

			case "response.incomplete":
				applyResponsesUsageToAnthropicState(state, gjson.Get(data, "response.usage"))
				terminalEventReceived = true
				reason := gjson.Get(data, "response.incomplete_details.reason").String()
				if reason != "max_output_tokens" {
					streamErr = fmt.Errorf("响应不完整: %s", reason)
				}
			}
		} else if raw := strings.TrimSpace(string(line)); raw != "" {
			eventType := streamDiagnosticEventType(raw)
			switch eventType {
			case "response.failed", "error", "response.incomplete":
				timing.observe(eventType, []byte(raw))
				terminalEventReceived = true
				streamErr = parseResponsesFailureEvent(eventType, []byte(raw))
				if streamErr == nil {
					streamErr = fmt.Errorf("上游错误: %s", raw)
				}
				skipCurrentOutput = true
			}
		}

		output := ""
		if !skipCurrentOutput {
			output = convertResponsesEventToAnthropic(line, originalRequest, state, model)
		}
		if output != "" {
			// 大事件诊断：翻译后的单条输出超阈值时打印源 type 与长度。
			if len(output) >= largeSSEEventThreshold {
				srcType := ""
				if data, ok := extractSSEData(string(line)); ok {
					srcType = streamDiagnosticEventType(data)
				}
				slog.Warn("[Anthropic SSE 大事件]",
					"src_type", srcType,
					"output_bytes", len(output),
				)
			}
			outputWritten = true
			_, _ = fmt.Fprint(w, output)
			if flusher != nil {
				flusher.Flush()
			}
		}

		// 错误事件已输出给客户端，现在终止流
		if streamErr != nil {
			goto done
		}
	}

done:
	if compactIdleTimedOut.Load() {
		streamErr = fmt.Errorf("compact summary upstream SSE idle timeout after %s", compactEventIdleTimeout)
	} else if err := scanner.Err(); err != nil && streamErr == nil {
		streamErr = fmt.Errorf("读取上游 SSE 失败: %w", err)
	}
	if streamErr == nil && !terminalEventReceived {
		streamErr = fmt.Errorf("上游 SSE 在完成事件前结束")
	}

	elapsed := time.Since(start)
	serviceTier = firstNonEmptyTier(serviceTier, defaultServiceTier)
	usage := newTokenUsage(
		billingModel,
		serviceTier,
		state.InputTokens,
		state.OutputTokens,
		state.CachedInputTokens,
		state.CacheCreationTokens,
		state.ReasoningOutputTokens,
		timing.firstEventMs,
	)
	usage.FirstTokenMs = timing.firstTokenMs

	// 即使流中断 / 上游 response.failed，中断前已产生的 token 上游也已实际计费，
	// 仍需回传 usage 计费避免漏洞（与 forward.go WS 路径同一模式）。
	abortUsage := func() *sdk.Usage {
		if state.InputTokens > 0 || state.OutputTokens > 0 || state.CachedInputTokens > 0 || state.CacheCreationTokens > 0 {
			fillUsageCost(usage)
			return usage
		}
		return nil
	}
	if streamErr != nil {
		var failure *responsesFailureError
		hasFailure := errors.As(streamErr, &failure)
		if !terminalEventReceived {
			output := closeOpenAnthropicContentBlocks(state)
			if hasFailure {
				output += buildAnthropicStreamErrorWithCode(failure.AnthropicErrorType, failure.Code, failure.Message)
			} else {
				output += buildAnthropicStreamError("api_error", streamErr.Error())
			}
			_, _ = fmt.Fprint(w, output)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if hasFailure {
			kind := failure.outcomeKind()
			// 流已开写后上游 response.failed：除 ClientError 外都按 StreamAborted 报告
			if kind != sdk.OutcomeClientError {
				kind = sdk.OutcomeStreamAborted
			}
			errBody := anthropicErrorJSONWithCode(failure.AnthropicErrorType, failure.Code, failure.Message)
			return sdk.ForwardOutcome{
				Kind:           kind,
				FailoverScope:  failure.failoverScopeForKind(kind),
				Upstream:       sdk.UpstreamResponse{StatusCode: failure.StatusCode, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: errBody},
				Reason:         failure.upstreamReason(),
				RetryAfter:     failure.RetryAfter,
				Duration:       elapsed,
				Usage:          abortUsage(),
				SafetyRejected: failure.isCybersecurityRisk(),
			}, continuationAnchorReplayErr(failure, outputWritten)
		}
		errBody := anthropicErrorJSON("api_error", streamErr.Error())
		return sdk.ForwardOutcome{
			Kind:           sdk.OutcomeStreamAborted,
			Upstream:       sdk.UpstreamResponse{StatusCode: http.StatusBadGateway, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: errBody},
			Reason:         streamErr.Error(),
			Duration:       elapsed,
			Usage:          abortUsage(),
			SafetyRejected: isCybersecurityRiskRejectionText(streamErr.Error()),
		}, streamErr
	}

	fillUsageCost(usage)
	setUsageResponseID(usage, responseID)
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK},
		Usage:    usage,
		Duration: elapsed,
	}, nil
}
