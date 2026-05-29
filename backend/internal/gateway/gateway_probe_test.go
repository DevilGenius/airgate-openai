package gateway

import (
	"encoding/json"
	"testing"
)

func TestBuildOAuthUsageProbeBodyUsesMiniModel(t *testing.T) {
	t.Parallel()

	var body struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Store        bool   `json:"store"`
		Stream       bool   `json:"stream"`
	}
	if err := json.Unmarshal(buildOAuthUsageProbeBody(), &body); err != nil {
		t.Fatalf("probe body should be valid JSON: %v", err)
	}
	if body.Model != oauthUsageProbeModel {
		t.Fatalf("model = %q, want %q", body.Model, oauthUsageProbeModel)
	}
	if body.Model != "gpt-5.4-mini" {
		t.Fatalf("probe model regressed to %q, want gpt-5.4-mini", body.Model)
	}
	if body.Instructions == "" {
		t.Fatalf("instructions should not be empty")
	}
	if body.Store {
		t.Fatalf("store = true, want false")
	}
	if !body.Stream {
		t.Fatalf("stream = false, want true")
	}
}
