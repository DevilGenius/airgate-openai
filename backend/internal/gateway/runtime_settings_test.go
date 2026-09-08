package gateway

import "testing"

func TestRuntimeSettingsAreHandledInsideGateway(t *testing.T) {
	g := &OpenAIGateway{}
	if err := g.ApplyRuntimeSettings(t.Context(), map[string]string{"text_hash_enabled": "false", "image_hash_enabled": "true", "unrelated-setting": "ignored"}); err != nil {
		t.Fatal(err)
	}
	state := g.runtimeHash.State()
	if state.TextEnabled || !state.ImageEnabled {
		t.Fatal(state)
	}
	if err := g.ApplyRuntimeSettings(t.Context(), map[string]string{"text_hash_enabled": "invalid"}); err == nil {
		t.Fatal("invalid runtime setting accepted")
	}
}
