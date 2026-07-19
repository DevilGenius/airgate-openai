package gateway

import (
	"reflect"
	"testing"
)

func TestAnthropicDefaultTargetModels(t *testing.T) {
	tests := []struct {
		name         string
		gotPrimary   string
		wantPrimary  string
		gotFallback  string
		wantFallback string
	}{
		{name: "fable", gotPrimary: fableTargetModel, wantPrimary: "gpt-5.6-sol", gotFallback: fableFallbackModel, wantFallback: "gpt-5.4"},
		{name: "opus", gotPrimary: opusTargetModel, wantPrimary: "gpt-5.6-terra", gotFallback: opusFallbackModel, wantFallback: "gpt-5.5"},
		{name: "sonnet", gotPrimary: sonnetTargetModel, wantPrimary: "gpt-5.6-luna", gotFallback: sonnetFallbackModel, wantFallback: "gpt-5.4"},
		{name: "haiku", gotPrimary: haikuTargetModel, wantPrimary: "gpt-5.3-codex-spark", gotFallback: haikuFallbackModel, wantFallback: "gpt-5.4-mini"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.gotPrimary != tt.wantPrimary {
				t.Fatalf("primary model = %q, want %q", tt.gotPrimary, tt.wantPrimary)
			}
			if tt.gotFallback != tt.wantFallback {
				t.Fatalf("fallback model = %q, want %q", tt.gotFallback, tt.wantFallback)
			}
		})
	}
}

func TestResolveAnthropicModelMapping_UsesUpdatedDefaultClaudeTarget(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{name: "unknown model fallback", model: "claude-foo-9"},
		{name: "claude 3 wildcard fallback", model: "claude-3-7-legacy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapping := resolveAnthropicModelMapping(tt.model)
			if mapping == nil {
				t.Fatal("mapping is nil")
			}
			if mapping.OpenAIModel != "gpt-5.5" {
				t.Fatalf("OpenAIModel = %q, want %q", mapping.OpenAIModel, "gpt-5.5")
			}
		})
	}
}

func TestResolveAnthropicModelMapping_UsesUnifiedPolicies(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantModel string
	}{
		{name: "fable", model: "claude-fable-1", wantModel: fableTargetModel},
		{name: "haiku", model: "claude-haiku-4-5-20251001", wantModel: haikuTargetModel},
		{name: "sonnet 4", model: "claude-sonnet-4-6", wantModel: sonnetTargetModel},
		{name: "legacy sonnet", model: "claude-sonnet-3-7", wantModel: sonnetTargetModel},
		{name: "opus 4", model: "claude-opus-4-8", wantModel: opusTargetModel},
		{name: "legacy opus", model: "claude-opus-3", wantModel: opusTargetModel},
		{name: "generic claude", model: "claude-3-7-legacy", wantModel: defaultClaudeTargetModel},
		{name: "unknown", model: "custom-anthropic-model", wantModel: defaultClaudeTargetModel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapping := resolveAnthropicModelMapping(tt.model)
			if mapping.OpenAIModel != tt.wantModel {
				t.Fatalf("OpenAIModel = %q, want %q", mapping.OpenAIModel, tt.wantModel)
			}
			if mapping.ReasoningEffort != defaultAnthropicReasoningEffort {
				t.Fatalf("ReasoningEffort = %q, want %q", mapping.ReasoningEffort, defaultAnthropicReasoningEffort)
			}
		})
	}
}

func TestAnthropicDispatchRules_UseUnifiedPolicies(t *testing.T) {
	rules := anthropicDispatchRules()
	if len(rules) != len(anthropicModelPolicies) {
		t.Fatalf("rules length = %d, want %d", len(rules), len(anthropicModelPolicies))
	}

	for i, policy := range anthropicModelPolicies {
		rule := rules[i]
		if rule.ID != policy.RuleID {
			t.Fatalf("rule[%d].ID = %q, want %q", i, rule.ID, policy.RuleID)
		}
		if !reflect.DeepEqual(rule.When.ModelPrefixes, policy.ModelPrefixes) {
			t.Fatalf("rule[%d].ModelPrefixes = %#v, want %#v", i, rule.When.ModelPrefixes, policy.ModelPrefixes)
		}
		wantCandidates := dispatchCandidates(policy.PrimaryModel, policy.FallbackModel)
		if !reflect.DeepEqual(rule.Candidates, wantCandidates) {
			t.Fatalf("rule[%d].Candidates = %#v, want %#v", i, rule.Candidates, wantCandidates)
		}
	}
}
