package gateway

import (
	"strings"

	"github.com/DevilGenius/airgate-openai/backend/internal/model"
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
		model.DefaultModelID,
	)
	fableTargetModel = resolveRoleTargetModel(
		model.DefaultFableModelID,
		"AIRGATE_MODEL_FABLE",
		"ANTHROPIC_DEFAULT_FABLE_MODEL",
	)
	opusTargetModel = resolveRoleTargetModel(
		model.DefaultOpusModelID,
		"AIRGATE_MODEL_OPUS",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
	)
	sonnetTargetModel = resolveRoleTargetModel(
		model.DefaultSonnetModelID,
		"AIRGATE_MODEL_SONNET",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
	)
	haikuTargetModel = resolveRoleTargetModel(
		model.DefaultHaikuModelID,
		"AIRGATE_MODEL_HAIKU",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	)
	enableAnthropicContinuation = strings.EqualFold(firstNonEmptyEnv("AIRGATE_ENABLE_ANTHROPIC_CONTINUATION"), "true")
)

// anthropicModelPolicies 按顺序匹配；具体家族必须位于 claude- 兜底规则之前。
var anthropicModelPolicies = []anthropicModelPolicy{
	{
		RuleID:                 "anthropic-fable",
		ModelPrefixes:          []string{"claude-fable-"},
		PrimaryModel:           fableTargetModel,
		FallbackModel:          model.DefaultModelID,
		DefaultReasoningEffort: defaultAnthropicReasoningEffort,
	},
	{
		RuleID:                 "anthropic-haiku",
		ModelPrefixes:          []string{"claude-haiku-"},
		PrimaryModel:           haikuTargetModel,
		DefaultReasoningEffort: defaultAnthropicReasoningEffort,
	},
	{
		RuleID:                 "anthropic-sonnet",
		ModelPrefixes:          []string{"claude-sonnet-"},
		PrimaryModel:           sonnetTargetModel,
		FallbackModel:          model.DefaultModelID,
		DefaultReasoningEffort: defaultAnthropicReasoningEffort,
	},
	{
		RuleID:                 "anthropic-opus",
		ModelPrefixes:          []string{"claude-opus-"},
		PrimaryModel:           opusTargetModel,
		FallbackModel:          model.DefaultModelID,
		DefaultReasoningEffort: defaultAnthropicReasoningEffort,
	},
	{
		RuleID:                 "anthropic-default",
		ModelPrefixes:          []string{"claude-"},
		PrimaryModel:           defaultClaudeTargetModel,
		FallbackModel:          model.DefaultModelID,
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
