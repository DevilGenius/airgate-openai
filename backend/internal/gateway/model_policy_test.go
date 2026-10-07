package gateway

import "testing"

func TestModelPolicyDSLDeclaresAliasesAndFallbacks(t *testing.T) {
	t.Setenv(longContextModelEnv, "gpt-long-configured")
	rules := openAIDispatchDSL().Rules
	found := map[string]bool{}
	for _, rule := range rules {
		found[rule.ID] = true
		switch rule.ID {
		case "images-generate-sunburst", "images-edit-sunburst":
			if len(rule.When.Models) != 1 || rule.When.Models[0] != "gpt-image-2.5" || len(rule.Candidates) != 1 || rule.Candidates[0].Wire != "gpt-image-2.5-sunburst" || rule.Gate.RequiredOperation == "" || rule.TimeoutProfile != "image" {
				t.Fatalf("incomplete image alias decision: %+v", rule)
			}
		case "responses-default", "chat-completions":
			if rule.Candidates[0].Wire != "${model}" || rule.ContextWindowFallback != "gpt-long-configured" {
				t.Fatalf("invalid identity/fallback: %+v", rule)
			}
		}
		if rule.Operation == "messages.generate" && rule.ContextWindowFallback != "gpt-long-configured" {
			t.Fatalf("Claude fallback absent: %+v", rule)
		}
	}
	for _, id := range []string{"images-generate-sunburst", "images-edit-sunburst", "responses-default", "chat-completions", "anthropic-fable", "anthropic-haiku", "anthropic-sonnet", "anthropic-opus", "anthropic-default"} {
		if !found[id] {
			t.Fatalf("missing rule %s", id)
		}
	}
}

func TestAnthropicModelPolicyCandidates(t *testing.T) {
	expected := map[string]string{"anthropic-fable": "gpt-6-astra", "anthropic-haiku": "gpt-5.6-luna", "anthropic-sonnet": "gpt-5.6-terra", "anthropic-opus": "gpt-5.6-sol", "anthropic-default": "gpt-5.6-sol"}
	for _, rule := range anthropicDispatchRules() {
		if rule.Candidates[0].Wire != expected[rule.ID] {
			t.Fatalf("unexpected candidate %+v", rule)
		}
		if rule.ID == "anthropic-fable" || rule.ID == "anthropic-sonnet" {
			if len(rule.Candidates) != 2 || rule.Candidates[1].Wire != "gpt-5.6-sol" {
				t.Fatalf("missing fallback %+v", rule)
			}
		}
	}
}
