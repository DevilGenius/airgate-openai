package gateway

import sdk "github.com/DevilGenius/airgate-sdk/sdkgo"

const compactModelSuffix = "-openai-compact"

func openAIDispatchDSL() sdk.DispatchDSL {
	return sdk.DispatchDSL{
		Rules: []sdk.DispatchRule{
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
			{
				ID:        "anthropic-fable",
				Operation: "messages.generate",
				When: sdk.DispatchWhen{
					Methods:       []string{"POST"},
					PathPrefixes:  []string{"/v1/messages", "/messages"},
					ModelPrefixes: []string{"claude-fable-"},
				},
				Candidates: dispatchCandidates(
					resolveRoleTargetModel(defaultClaudeTargetModel, "AIRGATE_MODEL_FABLE", "ANTHROPIC_DEFAULT_FABLE_MODEL"),
					resolveRoleTargetModel("gpt-5.4", "AIRGATE_MODEL_FABLE_FALLBACK"),
				),
			},
			{
				ID:        "anthropic-haiku",
				Operation: "messages.generate",
				When: sdk.DispatchWhen{
					Methods:       []string{"POST"},
					PathPrefixes:  []string{"/v1/messages", "/messages"},
					ModelPrefixes: []string{"claude-haiku-"},
				},
				Candidates: dispatchCandidates(
					resolveRoleTargetModel("gpt-5.3-codex-spark", "AIRGATE_MODEL_HAIKU", "ANTHROPIC_DEFAULT_HAIKU_MODEL"),
					resolveRoleTargetModel("gpt-5.4-mini", "AIRGATE_MODEL_HAIKU_FALLBACK"),
				),
			},
			{
				ID:        "anthropic-sonnet",
				Operation: "messages.generate",
				When: sdk.DispatchWhen{
					Methods:       []string{"POST"},
					PathPrefixes:  []string{"/v1/messages", "/messages"},
					ModelPrefixes: []string{"claude-sonnet-"},
				},
				Candidates: dispatchCandidates(
					resolveRoleTargetModel(defaultClaudeTargetModel, "AIRGATE_MODEL_SONNET", "ANTHROPIC_DEFAULT_SONNET_MODEL"),
					resolveRoleTargetModel("gpt-5.4", "AIRGATE_MODEL_SONNET_FALLBACK"),
				),
			},
			{
				ID:        "anthropic-opus",
				Operation: "messages.generate",
				When: sdk.DispatchWhen{
					Methods:       []string{"POST"},
					PathPrefixes:  []string{"/v1/messages", "/messages"},
					ModelPrefixes: []string{"claude-opus-"},
				},
				Candidates: dispatchCandidates(
					resolveRoleTargetModel(defaultClaudeTargetModel, "AIRGATE_MODEL_OPUS", "ANTHROPIC_DEFAULT_OPUS_MODEL"),
					resolveRoleTargetModel("gpt-5.4", "AIRGATE_MODEL_OPUS_FALLBACK"),
				),
			},
			{
				ID:        "anthropic-default",
				Operation: "messages.generate",
				When: sdk.DispatchWhen{
					Methods:       []string{"POST"},
					PathPrefixes:  []string{"/v1/messages", "/messages"},
					ModelPrefixes: []string{"claude-"},
				},
				Candidates: dispatchCandidates(
					defaultClaudeTargetModel,
					resolveRoleTargetModel("gpt-5.4", "AIRGATE_MODEL_DEFAULT_FALLBACK"),
				),
			},
			{
				ID:        "chat-completions",
				Operation: "chat.generate",
				When: sdk.DispatchWhen{
					Methods: []string{"POST"},
					Paths:   []string{"/v1/chat/completions", "/chat/completions"},
				},
				Candidates: identityDispatchCandidates(),
			},
			{
				ID:        "responses-default",
				Operation: "chat.generate",
				When: sdk.DispatchWhen{
					Methods: []string{"POST", "WS"},
					Paths:   []string{"/v1/responses", "/responses"},
				},
				Candidates: identityDispatchCandidates(),
			},
		},
	}
}

func identityDispatchCandidates() []sdk.DispatchCandidate {
	return []sdk.DispatchCandidate{{Scheduling: "${model}", Wire: "${model}"}}
}

func responsesCompressionFallbackModel() string {
	return resolveRoleTargetModel("gpt-5.4", "AIRGATE_MODEL_RESPONSES_COMPRESSION_FALLBACK")
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
