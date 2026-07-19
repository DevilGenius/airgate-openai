package gateway

import sdk "github.com/DevilGenius/airgate-sdk/sdkgo"

const compactModelSuffix = "-openai-compact"

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
		{
			ID:        "responses-compact",
			Operation: "responses.compact",
			When: sdk.DispatchWhen{
				Methods: []string{"POST"},
				Paths:   []string{"/v1/responses/compact", "/responses/compact"},
			},
			Model: sdk.DispatchModel{StripSuffix: compactModelSuffix},
			Candidates: []sdk.DispatchCandidate{
				{Scheduling: "${model.base}", Wire: "${model.base}"},
			},
		},
	}

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
