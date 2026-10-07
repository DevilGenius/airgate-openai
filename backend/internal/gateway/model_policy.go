package gateway

import (
	"github.com/DevilGenius/airgate-openai/backend/internal/model"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"os"
	"strings"
)

// Platform model decisions are declared here and executed by Core dispatchresolver.

const (
	defaultLongContextModel = "gpt-5.6-sol"
	longContextModelEnv     = "AIRGATE_MODEL_LONG_CONTEXT"
)

// configuredLongContextModel 返回 context window 超限后的统一重路由目标。环境变量允许在
// 模型下线或替换时直接切换，不需要修改请求缓存或调度控制逻辑。
func configuredLongContextModel() string {
	return resolveRoleTargetModel(defaultLongContextModel, longContextModelEnv)
}

func resolveRoleTargetModel(fallback string, keys ...string) string {
	return normalizeModelID(firstNonEmptyEnv(keys...), fallback)
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

// normalizeModelID 统一模型角色配置中的 provider 前缀和 @ 映射形式。
func normalizeModelID(raw string, fallback string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fallback
	}
	if idx := strings.LastIndex(value, "@"); idx >= 0 && idx+1 < len(value) {
		value = strings.TrimSpace(value[idx+1:])
	}
	for _, prefix := range []string{"openai/", "oai/"} {
		if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
			value = value[len(prefix):]
			break
		}
	}
	if value == "" {
		return fallback
	}
	return value
}

const defaultAnthropicReasoningEffort = "none"

// anthropicModelPolicy 是 Anthropic 模型路由的唯一配置来源。
// Dispatch DSL 与环境变量覆盖都从这里读取，协议转换不再查询模型映射。
type anthropicModelPolicy struct {
	RuleID        string
	ModelPrefixes []string
	PrimaryModel  string
	FallbackModel string
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
		RuleID:        "anthropic-fable",
		ModelPrefixes: []string{"claude-fable-"},
		PrimaryModel:  fableTargetModel,
		FallbackModel: model.DefaultModelID,
	},
	{
		RuleID:        "anthropic-haiku",
		ModelPrefixes: []string{"claude-haiku-"},
		PrimaryModel:  haikuTargetModel,
	},
	{
		RuleID:        "anthropic-sonnet",
		ModelPrefixes: []string{"claude-sonnet-"},
		PrimaryModel:  sonnetTargetModel,
		FallbackModel: model.DefaultModelID,
	},
	{
		RuleID:        "anthropic-opus",
		ModelPrefixes: []string{"claude-opus-"},
		PrimaryModel:  opusTargetModel,
		FallbackModel: model.DefaultModelID,
	},
	{
		RuleID:        "anthropic-default",
		ModelPrefixes: []string{"claude-"},
		PrimaryModel:  defaultClaudeTargetModel,
		FallbackModel: model.DefaultModelID,
	},
}

func openAIDispatchDSL() sdk.DispatchDSL {
	rules := []sdk.DispatchRule{
		{
			ID:        "images-generate",
			Operation: "images.generate",
			When: sdk.DispatchWhen{
				Methods: []string{"POST"},
				Paths:   []string{"/v1/images/generations", "/images/generations"},
			},
			TimeoutProfile: "image",
			Gate: sdk.DispatchGate{
				RequiredOperation: "images.generate",
				Status:            403,
				ErrorType:         "invalid_request_error",
				Code:              "images_generate_disabled",
				Message:           "Image generation is not enabled for this group",
			},
			Candidates: identityDispatchCandidates(),
		},
		{
			ID:        "images-edit",
			Operation: "images.edit",
			When: sdk.DispatchWhen{
				Methods: []string{"POST"},
				Paths:   []string{"/v1/images/edits", "/images/edits"},
			},
			TimeoutProfile: "image",
			Gate: sdk.DispatchGate{
				RequiredOperation: "images.edit",
				Status:            403,
				ErrorType:         "invalid_request_error",
				Code:              "images_edit_disabled",
				Message:           "Image generation is not enabled for this group",
			},
			Candidates: identityDispatchCandidates(),
		},
	}

	// Alias decisions happen before account selection, preserving gate and timeout.
	aliases := make([]sdk.DispatchRule, 0, 2)
	for _, rule := range rules {
		if rule.Operation != "images.generate" && rule.Operation != "images.edit" {
			continue
		}
		rule.ID += "-sunburst"
		rule.When.Models = []string{"gpt-image-2.5"}
		rule.Candidates = dispatchCandidates("gpt-image-2.5-sunburst")
		aliases = append(aliases, rule)
	}
	rules = append(aliases, rules...)
	rules = append(rules, anthropicDispatchRules()...)
	rules = append(rules,
		sdk.DispatchRule{
			ID:        "chat-completions",
			Operation: "chat.generate",
			When: sdk.DispatchWhen{
				Methods: []string{"POST"},
				Paths:   []string{"/v1/chat/completions", "/chat/completions"},
			},
			Candidates: identityDispatchCandidates(),
		},
		sdk.DispatchRule{
			ID:        "responses-default",
			Operation: "chat.generate",
			When: sdk.DispatchWhen{
				Methods: []string{"POST", "WS"},
				Paths:   []string{"/v1/responses", "/responses"},
			},
			Candidates: identityDispatchCandidates(),
		},
	)

	for i := range rules {
		if rules[i].Operation == "chat.generate" || rules[i].Operation == "messages.generate" {
			rules[i].ContextWindowFallback = configuredLongContextModel()
		}
	}
	return sdk.DispatchDSL{Rules: rules}
}

func anthropicDispatchRules() []sdk.DispatchRule {
	rules := make([]sdk.DispatchRule, 0, len(anthropicModelPolicies))
	for _, policy := range anthropicModelPolicies {
		rules = append(rules, sdk.DispatchRule{
			ID:        policy.RuleID,
			Operation: "messages.generate",
			When: sdk.DispatchWhen{
				Methods:       []string{"POST"},
				PathPrefixes:  []string{"/v1/messages", "/messages"},
				ModelPrefixes: append([]string(nil), policy.ModelPrefixes...),
			},
			Candidates: dispatchCandidates(policy.PrimaryModel, policy.FallbackModel),
		})
	}
	return rules
}

func identityDispatchCandidates() []sdk.DispatchCandidate {
	return []sdk.DispatchCandidate{{Scheduling: "${model}", Wire: "${model}"}}
}

func dispatchCandidates(models ...string) []sdk.DispatchCandidate {
	out := make([]sdk.DispatchCandidate, 0, len(models))
	seen := map[string]struct{}{}
	for _, model := range models {
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, sdk.DispatchCandidate{Scheduling: model, Wire: model})
	}
	return out
}
