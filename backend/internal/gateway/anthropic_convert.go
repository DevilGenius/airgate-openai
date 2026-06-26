package gateway

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ──────────────────────────────────────────────────────
// Anthropic → Responses API 一步直转（纯 gjson/sjson，零 struct）
// 参考 CLIProxyAPI translator/codex/claude/codex_claude_request.go
// ──────────────────────────────────────────────────────

// convertAnthropicRequestToResponses 将 Anthropic Messages API JSON 请求一步转换为 Responses API JSON
// modelName: 映射后的上游模型名
// mappingEffort: 模型映射注入的 reasoning_effort（优先级最高）
func convertAnthropicRequestToResponses(rawJSON []byte, modelName, mappingEffort string) []byte {
	root := gjson.ParseBytes(rawJSON)
	template := `{"model":"","instructions":"","input":[]}`
	template, _ = sjson.Set(template, "model", modelName)

	// ─── system → developer 消息（对齐 CLIProxyAPI：放入 input 数组而非 instructions 字段）───
	systemResult := root.Get("system")
	if systemResult.IsArray() {
		message := `{"type":"message","role":"developer","content":[]}`
		contentIndex := 0
		for _, item := range systemResult.Array() {
			if item.Get("type").String() == "text" {
				text := item.Get("text").String()
				if strings.HasPrefix(text, "x-anthropic-billing-header: ") {
					continue
				}
				if text != "" {
					message, _ = sjson.Set(message, fmt.Sprintf("content.%d.type", contentIndex), "input_text")
					message, _ = sjson.Set(message, fmt.Sprintf("content.%d.text", contentIndex), text)
					contentIndex++
				}
			}
		}
		if contentIndex > 0 {
			template, _ = sjson.SetRaw(template, "input.-1", message)
		}
	} else if systemResult.Type == gjson.String {
		if text := systemResult.String(); text != "" {
			message := `{"type":"message","role":"developer","content":[]}`
			message, _ = sjson.Set(message, "content.0.type", "input_text")
			message, _ = sjson.Set(message, "content.0.text", text)
			template, _ = sjson.SetRaw(template, "input.-1", message)
		}
	}

	// ─── messages → input[] ───
	toolNameMap := buildToolShortNameMapFromJSON(rawJSON)

	messagesResult := root.Get("messages")
	if messagesResult.IsArray() {
		for _, msgResult := range messagesResult.Array() {
			rawMsgRole := msgResult.Get("role").String()
			msgRole := normalizeAnthropicMessageRole(rawMsgRole)
			isSystemMessage := strings.EqualFold(strings.TrimSpace(rawMsgRole), "system")

			newMessage := func() string {
				msg := `{"type":"message","role":"","content":[]}`
				msg, _ = sjson.Set(msg, "role", msgRole)
				return msg
			}

			message := newMessage()
			contentIndex := 0
			hasContent := false

			flushMessage := func() {
				if hasContent {
					template, _ = sjson.SetRaw(template, "input.-1", message)
					message = newMessage()
					contentIndex = 0
					hasContent = false
				}
			}

			appendTextContent := func(text string) {
				if isSystemMessage && strings.HasPrefix(text, "x-anthropic-billing-header: ") {
					return
				}
				partType := "input_text"
				if msgRole == "assistant" {
					partType = "output_text"
				}
				message, _ = sjson.Set(message, fmt.Sprintf("content.%d.type", contentIndex), partType)
				message, _ = sjson.Set(message, fmt.Sprintf("content.%d.text", contentIndex), text)
				contentIndex++
				hasContent = true
			}

			appendImageContent := func(dataURL string) {
				message, _ = sjson.Set(message, fmt.Sprintf("content.%d.type", contentIndex), "input_image")
				message, _ = sjson.Set(message, fmt.Sprintf("content.%d.image_url", contentIndex), dataURL)
				contentIndex++
				hasContent = true
			}

			msgContents := msgResult.Get("content")
			if msgContents.IsArray() {
				for _, block := range msgContents.Array() {
					contentType := block.Get("type").String()
					switch contentType {
					// thinking / redacted_thinking：Anthropic extended thinking 产物，
					// 带 signature 字段，Responses API 无此概念。显式剥离避免上游拒绝，
					// 也防止 Claude Code 回传时被当成普通 content 注入 prompt。
					case "thinking", "redacted_thinking":
						continue

					case "text":
						appendTextContent(block.Get("text").String())

					case "image":
						source := block.Get("source")
						if source.Exists() {
							data := source.Get("data").String()
							if data == "" {
								data = source.Get("base64").String()
							}
							if data != "" {
								mediaType := source.Get("media_type").String()
								if mediaType == "" {
									mediaType = source.Get("mime_type").String()
								}
								if mediaType == "" {
									mediaType = "application/octet-stream"
								}
								appendImageContent(fmt.Sprintf("data:%s;base64,%s", mediaType, data))
							}
						}

					case "tool_use":
						flushMessage()
						fcMsg := `{"type":"function_call"}`
						fcMsg, _ = sjson.Set(fcMsg, "call_id", block.Get("id").String())
						name := block.Get("name").String()
						if short, ok := toolNameMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
						fcMsg, _ = sjson.Set(fcMsg, "name", name)
						if inputRaw := block.Get("input").Raw; inputRaw != "" {
							fcMsg, _ = sjson.Set(fcMsg, "arguments", inputRaw)
						} else {
							fcMsg, _ = sjson.Set(fcMsg, "arguments", "{}")
						}
						template, _ = sjson.SetRaw(template, "input.-1", fcMsg)

					case "tool_result":
						flushMessage()
						fcoMsg := `{"type":"function_call_output"}`
						fcoMsg, _ = sjson.Set(fcoMsg, "call_id", block.Get("tool_use_id").String())

						outputText, imageURLs := anthropicToolResultOutput(block.Get("content"))
						fcoMsg, _ = sjson.Set(fcoMsg, "output", outputText)

						// is_error 标记
						if block.Get("is_error").Bool() {
							// 在 output 前加 [tool_error] 标记
							if out := gjson.Get(fcoMsg, "output"); out.Type == gjson.String {
								fcoMsg, _ = sjson.Set(fcoMsg, "output", "[tool_error] "+out.String())
							}
						}

						template, _ = sjson.SetRaw(template, "input.-1", fcoMsg)
						if len(imageURLs) > 0 {
							imageMessage := newMessage()
							for imageIndex, imageURL := range imageURLs {
								imageMessage, _ = sjson.Set(imageMessage, fmt.Sprintf("content.%d.type", imageIndex), "input_image")
								imageMessage, _ = sjson.Set(imageMessage, fmt.Sprintf("content.%d.image_url", imageIndex), imageURL)
							}
							template, _ = sjson.SetRaw(template, "input.-1", imageMessage)
						}
					}
				}
				flushMessage()
			} else if msgContents.Type == gjson.String {
				appendTextContent(msgContents.String())
				flushMessage()
			}
		}
	}

	// ─── tools → tools[] ───
	toolsResult := root.Get("tools")
	convertedToolNames := map[string]struct{}{}
	hasConvertedWebSearchTool := false
	discoveredDeferredTools := collectAnthropicDiscoveredToolNames(root)
	forcedToolChoiceName := ""
	if tc := root.Get("tool_choice"); tc.Exists() && tc.IsObject() && strings.TrimSpace(tc.Get("type").String()) == "tool" {
		forcedToolChoiceName = tc.Get("name").String()
	}
	if toolsResult.IsArray() {
		template, _ = sjson.SetRaw(template, "tools", `[]`)

		var names []string
		for _, t := range toolsResult.Array() {
			if n := t.Get("name").String(); n != "" {
				names = append(names, n)
			}
		}
		shortMap := buildShortNameMap(names)

		for _, toolResult := range toolsResult.Array() {
			// web_search 特殊处理
			toolType := toolResult.Get("type").String()
			if toolType == "web_search_20250305" || toolType == "web_search" {
				template, _ = sjson.SetRaw(template, "tools.-1", `{"type":"web_search"}`)
				hasConvertedWebSearchTool = true
				continue
			}

			if isAnthropicToolSearchType(toolType) {
				continue
			}
			if toolType != "" && toolType != "custom" && toolType != "function" {
				continue
			}

			originalName := toolResult.Get("name").String()
			if originalName == "" {
				continue
			}
			if isAnthropicDeferredTool(toolResult) && originalName != forcedToolChoiceName {
				if _, ok := discoveredDeferredTools[originalName]; !ok {
					continue
				}
			}

			name := originalName
			if short, ok := shortMap[name]; ok {
				name = short
			} else {
				name = shortenNameIfNeeded(name)
			}

			tool := `{"type":"function"}`
			tool, _ = sjson.Set(tool, "name", name)
			if desc := toolResult.Get("description").String(); desc != "" {
				tool, _ = sjson.Set(tool, "description", desc)
			}

			schemaRaw := toolResult.Get("input_schema").Raw
			if strings.TrimSpace(schemaRaw) == "" {
				schemaRaw = toolResult.Get("parameters").Raw
			}
			tool, _ = sjson.SetRaw(tool, "parameters", normalizeToolParametersJSON(schemaRaw))
			tool, _ = sjson.Delete(tool, "parameters.$schema")
			tool, _ = sjson.Set(tool, "strict", false)

			template, _ = sjson.SetRaw(template, "tools.-1", tool)
			convertedToolNames[name] = struct{}{}
		}
	}

	// ─── tool_choice 转换 ───
	if tc := root.Get("tool_choice"); tc.Exists() && tc.IsObject() {
		tcType := strings.TrimSpace(tc.Get("type").String())
		switch tcType {
		case "auto":
			template, _ = sjson.Set(template, "tool_choice", "auto")
		case "none":
			template, _ = sjson.Set(template, "tool_choice", "none")
		case "any":
			if len(convertedToolNames) > 0 || hasConvertedWebSearchTool {
				template, _ = sjson.Set(template, "tool_choice", "required")
			} else {
				template, _ = sjson.Set(template, "tool_choice", "auto")
			}
		case "tool":
			name := tc.Get("name").String()
			if name == "web_search" || name == "web_search_20250305" {
				if hasConvertedWebSearchTool {
					template, _ = sjson.SetRaw(template, "tool_choice", `{"type":"web_search"}`)
				} else {
					template, _ = sjson.Set(template, "tool_choice", "auto")
				}
			} else {
				if short, ok := toolNameMap[name]; ok {
					name = short
				} else {
					name = shortenNameIfNeeded(name)
				}
				if _, ok := convertedToolNames[name]; !ok {
					template, _ = sjson.Set(template, "tool_choice", "auto")
				} else {
					tcJSON := `{"type":"function","name":""}`
					tcJSON, _ = sjson.Set(tcJSON, "name", name)
					template, _ = sjson.SetRaw(template, "tool_choice", tcJSON)
				}
			}
		default:
			template, _ = sjson.Set(template, "tool_choice", "auto")
		}
	} else if len(convertedToolNames) > 0 || hasConvertedWebSearchTool {
		template, _ = sjson.Set(template, "tool_choice", "auto")
	}

	// ─── thinking / output_config → reasoning_effort ───
	// 优先级：
	//   1. thinking.type=disabled       （显式关闭 → none）
	//   2. output_config.effort         （Claude Code Effort 滑块，thinking 未关闭时识别）
	//   3. thinking.budget_tokens       （Anthropic 原生 extended thinking 预算）
	//   4. mappingEffort                （模型映射默认）
	//   5. "medium"                     （全局兜底）
	reasoningEffort := "medium"
	clientEffortSet := false

	thinkingConfig := root.Get("thinking")
	if thinkingConfig.Exists() && thinkingConfig.IsObject() {
		if strings.TrimSpace(thinkingConfig.Get("type").String()) == "disabled" {
			reasoningEffort = "none"
			clientEffortSet = true
		}
	}

	if !clientEffortSet {
		if v := root.Get("output_config.effort"); v.Exists() && v.Type == gjson.String {
			if e := normalizeReasoningEffort(v.String()); e != "" {
				reasoningEffort = e
				clientEffortSet = true
			}
		}
	}

	if !clientEffortSet && thinkingConfig.Exists() && thinkingConfig.IsObject() {
		switch strings.TrimSpace(thinkingConfig.Get("type").String()) {
		case "enabled":
			if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
				if effort := thinkingBudgetToReasoningEffort(budgetTokens.Int()); effort != "" {
					reasoningEffort = effort
					clientEffortSet = true
				}
			}
		case "adaptive", "auto":
			// adaptive 已在 output_config.effort 阶段消费；未传 effort 时让映射默认生效。
		}
	}

	// 4. 客户端未指定 → 使用模型映射的默认 effort
	if !clientEffortSet && mappingEffort != "" {
		if e := normalizeReasoningEffort(mappingEffort); e != "" {
			reasoningEffort = e
		} else {
			reasoningEffort = mappingEffort
		}
	}

	// ─── 固定参数（对齐 Codex CLI ResponsesApiRequest）───
	parallelToolCalls := true
	if disableParallelToolUse := root.Get("tool_choice.disable_parallel_tool_use"); disableParallelToolUse.Exists() {
		parallelToolCalls = !disableParallelToolUse.Bool()
	}
	template, _ = sjson.Set(template, "parallel_tool_calls", parallelToolCalls)
	template, _ = sjson.Set(template, "reasoning.effort", reasoningEffort)
	template, _ = sjson.Set(template, "reasoning.summary", "auto")
	template, _ = sjson.Set(template, "stream", true)
	template, _ = sjson.Set(template, "store", false)
	template, _ = sjson.Set(template, "include", []string{"reasoning.encrypted_content"})
	template, _ = sjson.Set(template, "text.verbosity", "medium") // 输出简练度（对齐 Codex CLI 默认值）

	// 注意：上游 Codex 风格 Responses 代理不接受 max_output_tokens 字段（会 400 Unsupported parameter），
	// Anthropic 的 max_tokens 在此被静默丢弃。如果以后切到原生 OpenAI Responses API，再恢复此映射。

	return []byte(template)
}

func anthropicToolResultOutput(content gjson.Result) (string, []string) {
	if !content.Exists() || content.Type == gjson.Null {
		return "(empty)", nil
	}
	if content.Type == gjson.String {
		text := content.String()
		if text == "" {
			text = "(empty)"
		}
		return text, nil
	}
	if !content.IsArray() {
		raw := strings.TrimSpace(content.Raw)
		if raw == "" {
			raw = "(empty)"
		}
		return raw, nil
	}

	textParts := []string{}
	imageURLs := []string{}
	for _, part := range content.Array() {
		switch part.Get("type").String() {
		case "text":
			if text := part.Get("text").String(); text != "" {
				textParts = append(textParts, text)
			}
		case "image":
			if imageURL := anthropicImageDataURL(part.Get("source")); imageURL != "" {
				imageURLs = append(imageURLs, imageURL)
			}
		case "tool_reference":
		default:
			if raw := strings.TrimSpace(part.Raw); raw != "" {
				textParts = append(textParts, raw)
			}
		}
	}

	text := strings.Join(textParts, "\n\n")
	if text == "" {
		text = "(empty)"
	}
	return text, imageURLs
}

func anthropicImageDataURL(source gjson.Result) string {
	if !source.Exists() {
		return ""
	}
	data := source.Get("data").String()
	if data == "" {
		data = source.Get("base64").String()
	}
	if data == "" {
		return ""
	}
	mediaType := source.Get("media_type").String()
	if mediaType == "" {
		mediaType = source.Get("mime_type").String()
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return fmt.Sprintf("data:%s;base64,%s", mediaType, data)
}

func normalizeAnthropicMessageRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system":
		return "developer"
	case "developer":
		return "developer"
	case "assistant":
		return "assistant"
	case "user":
		return "user"
	default:
		return role
	}
}

// convertAnthropicRequestToResponsesContinuation 将 Anthropic 请求压缩为 continuation 形式：
// 仅保留最后一条消息作为增量输入，并挂上 previous_response_id。
// 适用于已存在稳定会话锚点的 Claude Code / Messages 多轮请求。
func convertAnthropicRequestToResponsesContinuation(rawJSON []byte, modelName, mappingEffort, previousResponseID string) ([]byte, bool) {
	if strings.TrimSpace(previousResponseID) == "" {
		return nil, false
	}

	root := gjson.ParseBytes(rawJSON)
	messages := root.Get("messages")
	if !messages.IsArray() || messages.Get("#").Int() == 0 {
		return nil, false
	}

	items := messages.Array()
	last := items[len(items)-1]
	if last.Get("role").String() == "" {
		return nil, false
	}

	trimmed := `{"model":"","messages":[]}`
	trimmed, _ = sjson.Set(trimmed, "model", root.Get("model").String())
	trimmed, _ = sjson.Set(trimmed, "stream", root.Get("stream").Bool())
	if mt := root.Get("max_tokens"); mt.Exists() {
		trimmed, _ = sjson.Set(trimmed, "max_tokens", mt.Int())
	}
	if thinking := root.Get("thinking"); thinking.Exists() {
		trimmed, _ = sjson.SetRaw(trimmed, "thinking", thinking.Raw)
	}
	if outputConfig := root.Get("output_config"); outputConfig.Exists() {
		trimmed, _ = sjson.SetRaw(trimmed, "output_config", outputConfig.Raw)
	}
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		trimmed, _ = sjson.SetRaw(trimmed, "tool_choice", toolChoice.Raw)
	}
	if tools := root.Get("tools"); tools.Exists() {
		trimmed, _ = sjson.SetRaw(trimmed, "tools", tools.Raw)
	}
	trimmed, _ = sjson.SetRaw(trimmed, "messages.0", last.Raw)

	responsesBody := convertAnthropicRequestToResponses([]byte(trimmed), modelName, mappingEffort)
	responsesBody, _ = sjson.SetBytes(responsesBody, "previous_response_id", previousResponseID)
	return responsesBody, true
}

// ──────────────────────────────────────────────────────
// 请求验证（纯 gjson，不依赖 struct）
// ──────────────────────────────────────────────────────

// validateAnthropicRequestJSON 验证 Anthropic 请求 JSON 基本字段
// 返回 (statusCode, errType, errMsg) 或 (0, "", "") 表示验证通过
func validateAnthropicRequestJSON(body []byte) (int, string, string) {
	root := gjson.ParseBytes(body)

	if !root.Get("model").Exists() || root.Get("model").String() == "" {
		return 400, "invalid_request_error", "model is required"
	}
	if !root.Get("messages").Exists() || !root.Get("messages").IsArray() || root.Get("messages.#").Int() == 0 {
		return 400, "invalid_request_error", "messages is required"
	}
	if !root.Get("max_tokens").Exists() || root.Get("max_tokens").Int() <= 0 {
		return 400, "invalid_request_error", "max_tokens must be greater than 0"
	}

	// 验证 thinking
	if thinking := root.Get("thinking"); thinking.Exists() && thinking.IsObject() {
		switch thinking.Get("type").String() {
		case "enabled":
			if thinking.Get("budget_tokens").Int() <= 0 {
				return 400, "invalid_request_error", "budget_tokens is required when thinking type is enabled"
			}
		case "adaptive", "disabled":
			// ok
		default:
			return 400, "invalid_request_error", "thinking type must be one of: enabled, disabled, adaptive"
		}
	}

	// 验证 tool_choice
	if tc := root.Get("tool_choice"); tc.Exists() && tc.IsObject() {
		switch tc.Get("type").String() {
		case "auto", "none", "any":
			// ok
		case "tool":
			if tc.Get("name").String() == "" {
				return 400, "invalid_request_error", "name is required when tool_choice type is tool"
			}
		default:
			return 400, "invalid_request_error", "tool_choice type must be one of: auto, none, any, tool"
		}
	}

	return 0, "", ""
}
