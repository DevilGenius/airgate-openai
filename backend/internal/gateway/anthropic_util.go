package gateway

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ──────────────────────────────────────────────────────
// 工具函数（纯函数，不依赖任何 struct）
// ──────────────────────────────────────────────────────

// thinkingBudgetToReasoningEffort 将 thinking budget_tokens 映射为 reasoning_effort
// 返回值为上游 Codex Responses 接受的 effort 等级（none/low/medium/high/xhigh）
//   - budget < 0  : 返回 "" 表示未指定，调用方应使用兜底策略
//   - budget == 0 : 客户端显式禁用思考 → "none"
//   - 其余按预算大小档位映射
func thinkingBudgetToReasoningEffort(budget int64) string {
	switch {
	case budget < 0:
		return ""
	case budget == 0:
		return "none"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	default:
		return "xhigh"
	}
}

// normalizeReasoningEffort 把客户端传入的 effort 字符串归一化到合法集合
// 合法返回：none / low / medium / high / xhigh
// 无法识别返回 ""，由调用方走兜底逻辑
func normalizeReasoningEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "minimal", "min", "none", "off", "disabled":
		return "none"
	case "low":
		return "low"
	case "medium", "mid", "normal", "default":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "very_high", "veryhigh", "max", "maximum", "ultra":
		return "xhigh"
	default:
		return ""
	}
}

// normalizeAnthropicStopReason 归一化 OpenAI/Responses stop_reason，输出 Anthropic 兼容值。
func normalizeAnthropicStopReason(reason string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(reason)); normalized {
	case "", "stop":
		return "end_turn"
	case "length", "max_output_tokens":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "context_length_exceeded", "input_too_long":
		// OpenAI 上下文超限 → Claude Code 会触发 tengu_context_window_exceeded 遥测 +
		// 用户可见的 "response exceeded context window" 提示
		return "model_context_window_exceeded"
	default:
		return normalized
	}
}

// anthropicValidStopReasons Claude 官方 stop_reason 合法枚举
// 参考：https://docs.anthropic.com/en/api/messages
var anthropicValidStopReasons = map[string]struct{}{
	"end_turn":                      {},
	"max_tokens":                    {},
	"stop_sequence":                 {},
	"tool_use":                      {},
	"refusal":                       {},
	"pause_turn":                    {},
	"model_context_window_exceeded": {},
}

// ensureAnthropicStopReason 兜底白名单校验：如果 normalize 后的值不在合法集合里，
// 统一降级为 "end_turn"，避免把 OpenAI 内部的奇怪 reason 泄漏给客户端。
func ensureAnthropicStopReason(reason string) string {
	if _, ok := anthropicValidStopReasons[reason]; ok {
		return reason
	}
	return "end_turn"
}

// ──────────────────────────────────────────────────────
// 工具名缩短
// ──────────────────────────────────────────────────────

// shortenNameIfNeeded 按与 CLIProxyAPI 一致的规则缩短工具名
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			cand := "mcp__" + name[idx+2:]
			if len(cand) > limit {
				return cand[:limit]
			}
			return cand
		}
	}
	return name[:limit]
}

// buildShortNameMap 保证同一请求内缩短名唯一
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > limit {
					cand = cand[:limit]
				}
				return cand
			}
		}
		return n[:limit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}

// buildToolShortNameMapFromJSON 基于原始 JSON body 中的 tools 数组构建 original->short 映射
func buildToolShortNameMapFromJSON(body []byte) map[string]string {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return map[string]string{}
	}
	var names []string
	for _, t := range tools.Array() {
		if n := t.Get("name").String(); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return map[string]string{}
	}
	return buildShortNameMap(names)
}

// buildReverseToolNameMap 基于原始 Anthropic tools 构建 short->original 映射
func buildReverseToolNameMap(original []byte) map[string]string {
	rev := map[string]string{}
	for orig, short := range buildToolShortNameMapFromJSON(original) {
		rev[short] = orig
	}
	return rev
}

// hasWebSearchTool 检查 Anthropic 请求 JSON 中是否包含 web_search 原生工具
func hasWebSearchTool(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, t := range tools.Array() {
		switch t.Get("type").String() {
		case "web_search_20250305", "web_search":
			return true
		}
	}
	return false
}

func isAnthropicToolSearchType(toolType string) bool {
	return strings.HasPrefix(strings.TrimSpace(toolType), "tool_search")
}

func isAnthropicDeferredTool(tool gjson.Result) bool {
	return tool.Get("defer_loading").Bool() || tool.Get("custom.defer_loading").Bool()
}

func collectAnthropicDiscoveredToolNames(root gjson.Result) map[string]struct{} {
	discovered := map[string]struct{}{}
	messages := root.Get("messages")
	if !messages.IsArray() {
		return discovered
	}
	for _, msg := range messages.Array() {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}
		for _, block := range content.Array() {
			if block.Get("type").String() == "tool_result" {
				collectToolReferencesFromContent(block.Get("content"), discovered)
			}
		}
	}
	return discovered
}

func collectToolReferencesFromContent(content gjson.Result, discovered map[string]struct{}) {
	if content.IsArray() {
		for _, item := range content.Array() {
			if item.Get("type").String() == "tool_reference" {
				if name := strings.TrimSpace(item.Get("tool_name").String()); name != "" {
					discovered[name] = struct{}{}
				}
				continue
			}
			collectToolReferencesFromText(item.String(), discovered)
		}
		return
	}
	collectToolReferencesFromText(content.String(), discovered)
}

func collectToolReferencesFromText(text string, discovered map[string]struct{}) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if gjson.Valid(text) {
		collectToolReferencesFromParsedJSON(gjson.Parse(text), discovered)
		return
	}

	remaining := text
	for {
		start := strings.Index(remaining, "<function>")
		if start < 0 {
			return
		}
		remaining = remaining[start+len("<function>"):]
		end := strings.Index(remaining, "</function>")
		if end < 0 {
			return
		}
		candidate := strings.TrimSpace(remaining[:end])
		if gjson.Valid(candidate) {
			if name := strings.TrimSpace(gjson.Parse(candidate).Get("name").String()); name != "" {
				discovered[name] = struct{}{}
			}
		}
		remaining = remaining[end+len("</function>"):]
	}
}

func collectToolReferencesFromParsedJSON(value gjson.Result, discovered map[string]struct{}) {
	if value.IsArray() {
		for _, item := range value.Array() {
			collectToolReferencesFromParsedJSON(item, discovered)
		}
		return
	}
	if value.Get("type").String() == "tool_reference" {
		if name := strings.TrimSpace(value.Get("tool_name").String()); name != "" {
			discovered[name] = struct{}{}
		}
		return
	}
	if name := strings.TrimSpace(value.Get("name").String()); name != "" && value.Get("parameters").Exists() {
		discovered[name] = struct{}{}
	}
}

func estimateAnthropicInputTokens(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	if !gjson.ValidBytes(body) {
		return estimateTextTokens(string(body))
	}

	tokens := 8
	if system := gjson.GetBytes(body, "system"); system.Exists() {
		tokens += estimateAnthropicContentTokens(system) + 4
	}
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		tokens += 8 + estimateTextTokens(msg.Get("role").String())
		tokens += estimateAnthropicContentTokens(msg.Get("content"))
	}
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		tokens += 24 + estimateTextTokens(tool.Get("name").String())
		tokens += estimateTextTokens(tool.Get("description").String())
		if raw := tool.Get("input_schema").Raw; raw != "" {
			tokens += estimateTextTokens(raw)
		} else if raw := tool.Get("parameters").Raw; raw != "" {
			tokens += estimateTextTokens(raw)
		}
	}
	if raw := gjson.GetBytes(body, "tool_choice").Raw; raw != "" {
		tokens += estimateTextTokens(raw)
	}
	return tokens
}

func estimateAnthropicContentTokens(content gjson.Result) int {
	switch {
	case !content.Exists() || content.Type == gjson.Null:
		return 0
	case content.IsArray():
		tokens := 0
		for _, block := range content.Array() {
			tokens += 6
			switch block.Get("type").String() {
			case "text", "thinking":
				tokens += estimateTextTokens(block.Get("text").String())
				tokens += estimateTextTokens(block.Get("thinking").String())
			case "tool_use":
				tokens += 16 + estimateTextTokens(block.Get("name").String()) + estimateTextTokens(block.Get("input").Raw)
			case "tool_result":
				tokens += 12 + estimateAnthropicContentTokens(block.Get("content"))
			case "image", "document":
				tokens += estimateTextTokens(block.Raw) + 256
			default:
				tokens += estimateTextTokens(block.Raw)
			}
		}
		return tokens
	case content.Type == gjson.String:
		return estimateTextTokens(content.String())
	default:
		return estimateTextTokens(content.Raw)
	}
}

func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	asciiChars := 0
	nonASCIIChars := 0
	for _, r := range text {
		if r < 128 {
			asciiChars++
		} else {
			nonASCIIChars++
		}
	}
	tokens := (asciiChars + 3) / 4
	tokens += nonASCIIChars
	return tokens
}

// normalizeToolParametersJSON 确保 object schema 至少包含空 properties（参考 CLIProxyAPI normalizeToolParameters）
func normalizeToolParametersJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || !gjson.Valid(raw) {
		return `{"type":"object","properties":{}}`
	}
	schema := raw
	result := gjson.Parse(raw)
	schemaType := result.Get("type").String()
	if schemaType == "" {
		schema, _ = sjson.Set(schema, "type", "object")
		schemaType = "object"
	}
	if schemaType == "object" && !result.Get("properties").Exists() {
		schema, _ = sjson.SetRaw(schema, "properties", `{}`)
	}
	return schema
}

// extractResponsesUsage 从 Responses usage 中提取 token 统计（cached_tokens 从 input_tokens 扣除）
// 返回: inputTokens（不含缓存）, outputTokens, cachedTokens, reasoningTokens
func extractResponsesUsage(usage gjson.Result) (int64, int64, int64, int64) {
	if !usage.Exists() || usage.Type == gjson.Null {
		return 0, 0, 0, 0
	}
	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	cachedTokens := usage.Get("input_tokens_details.cached_tokens").Int()
	reasoningTokens := usage.Get("output_tokens_details.reasoning_tokens").Int()
	if cachedTokens > 0 {
		if inputTokens >= cachedTokens {
			inputTokens -= cachedTokens
		} else {
			inputTokens = 0
		}
	}
	return inputTokens, outputTokens, cachedTokens, reasoningTokens
}

// injectWebSearchToolJSON 向 Responses API JSON 请求体注入 web_search 工具
func injectWebSearchToolJSON(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, t := range tools.Array() {
			if t.Get("type").String() == "web_search" {
				return body // 已存在
			}
		}
	}
	result, err := sjson.SetRawBytes(body, "tools.-1", []byte(`{"type":"web_search"}`))
	if err != nil {
		return body
	}
	return result
}
