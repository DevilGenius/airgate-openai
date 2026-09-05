package gateway

import (
	"strings"
)

// ──────────────────────────────────────────────────────
// Claude → OpenAI 模型映射
// Claude Code 发送 Claude 模型名，翻译为 OpenAI 模型 + 额外参数
// ──────────────────────────────────────────────────────

// anthropicModelMapping 单条模型映射规则
type anthropicModelMapping struct {
	// OpenAIModel 映射到的 OpenAI 模型名
	OpenAIModel string
	// ReasoningEffort 默认的 reasoning_effort（客户端 thinking 配置优先）
	ReasoningEffort string
}

const defaultAnthropicReasoningEffort = "none"

// anthropicModelPolicy 是 Anthropic 模型路由的唯一配置来源。
// Dispatch DSL、请求转换默认 effort 和环境变量覆盖都从这里读取，避免多处重复维护。
type anthropicModelPolicy struct {
	RuleID                 string
	ModelPrefixes          []string
	PrimaryModel           string
	FallbackModel          string
	DefaultReasoningEffort string
}

var (
	defaultClaudeTargetModel = normalizeModelID(
		firstNonEmptyEnv("AIRGATE_DEFAULT_CLAUDE_MODEL"),
		"gpt-5.6-sol",
	)
	fableTargetModel = resolveRoleTargetModel(
		"gpt-6-astra",
		"AIRGATE_MODEL_FABLE",
		"ANTHROPIC_DEFAULT_FABLE_MODEL",
	)
	fableFallbackModel = resolveRoleTargetModel(
		"gpt-5.5",
		"AIRGATE_MODEL_FABLE_FALLBACK",
	)
	opusTargetModel = resolveRoleTargetModel(
		"gpt-5.6-sol",
		"AIRGATE_MODEL_OPUS",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
	)
	opusFallbackModel = resolveRoleTargetModel(
		"gpt-5.5",
		"AIRGATE_MODEL_OPUS_FALLBACK",
	)
	sonnetTargetModel = resolveRoleTargetModel(
		"gpt-5.6-terra",
		"AIRGATE_MODEL_SONNET",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
	)
	sonnetFallbackModel = resolveRoleTargetModel(
		"gpt-5.5",
		"AIRGATE_MODEL_SONNET_FALLBACK",
	)
	haikuTargetModel = resolveRoleTargetModel(
		"gpt-5.6-luna",
		"AIRGATE_MODEL_HAIKU",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	)
	haikuFallbackModel = resolveRoleTargetModel(
		"gpt-5.4-mini",
		"AIRGATE_MODEL_HAIKU_FALLBACK",
	)
	defaultClaudeFallbackModel = resolveRoleTargetModel(
		"gpt-5.5",
		"AIRGATE_MODEL_DEFAULT_FALLBACK",
	)
	// sparkTargetModel 简单操作加速模型（Read/Grep/Glob 结果处理时自动路由）
	// 空字符串表示禁用 Spark 路由
	sparkTargetModel = resolveRoleTargetModel(
		"gpt-5.3-codex-spark",
		"AIRGATE_MODEL_SPARK",
	)
	// codexDefaultModel Codex CLI 透传路径的兜底模型。
	// 当客户端请求体里 model 字段为空、null 或字面量 "None" 时使用这个值，
	// 默认使用 gpt-5.5，也可通过 AIRGATE_CODEX_DEFAULT_MODEL 覆盖。
	codexDefaultModel = resolveRoleTargetModel(
		"gpt-5.5",
		"AIRGATE_CODEX_DEFAULT_MODEL",
	)
	enableAnthropicContinuation = strings.EqualFold(firstNonEmptyEnv("AIRGATE_ENABLE_ANTHROPIC_CONTINUATION"), "true")
)

// anthropicModelPolicies 按顺序匹配；具体家族必须位于 claude- 兜底规则之前。
var anthropicModelPolicies = []anthropicModelPolicy{
	{
		RuleID:                 "anthropic-fable",
		ModelPrefixes:          []string{"claude-fable-"},
		PrimaryModel:           fableTargetModel,
		FallbackModel:          fableFallbackModel,
		DefaultReasoningEffort: defaultAnthropicReasoningEffort,
	},
	{
		RuleID:                 "anthropic-haiku",
		ModelPrefixes:          []string{"claude-haiku-"},
		PrimaryModel:           haikuTargetModel,
		FallbackModel:          haikuFallbackModel,
		DefaultReasoningEffort: defaultAnthropicReasoningEffort,
	},
	{
		RuleID:                 "anthropic-sonnet",
		ModelPrefixes:          []string{"claude-sonnet-"},
		PrimaryModel:           sonnetTargetModel,
		FallbackModel:          sonnetFallbackModel,
		DefaultReasoningEffort: defaultAnthropicReasoningEffort,
	},
	{
		RuleID:                 "anthropic-opus",
		ModelPrefixes:          []string{"claude-opus-"},
		PrimaryModel:           opusTargetModel,
		FallbackModel:          opusFallbackModel,
		DefaultReasoningEffort: defaultAnthropicReasoningEffort,
	},
	{
		RuleID:                 "anthropic-default",
		ModelPrefixes:          []string{"claude-"},
		PrimaryModel:           defaultClaudeTargetModel,
		FallbackModel:          defaultClaudeFallbackModel,
		DefaultReasoningEffort: defaultAnthropicReasoningEffort,
	},
}

// defaultModelMapping 兜底映射：不认识的 Claude 模型统一用默认 OpenAI 主模型。
var defaultModelMapping = anthropicModelMapping{
	OpenAIModel:     defaultClaudeTargetModel,
	ReasoningEffort: defaultAnthropicReasoningEffort,
}

func (p anthropicModelPolicy) matches(model string) bool {
	for _, prefix := range p.ModelPrefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

func resolveAnthropicModelPolicy(claudeModel string) *anthropicModelPolicy {
	for i := range anthropicModelPolicies {
		if anthropicModelPolicies[i].matches(claudeModel) {
			return &anthropicModelPolicies[i]
		}
	}
	return nil
}

// resolveAnthropicModelMapping 解析 Claude 模型名的映射
// 模型和 effort 均来自统一 policy，始终返回非 nil。
func resolveAnthropicModelMapping(claudeModel string) *anthropicModelMapping {
	if policy := resolveAnthropicModelPolicy(claudeModel); policy != nil {
		mapping := anthropicModelMapping{
			OpenAIModel:     policy.PrimaryModel,
			ReasoningEffort: policy.DefaultReasoningEffort,
		}
		return &mapping
	}

	m := defaultModelMapping
	return &m
}
