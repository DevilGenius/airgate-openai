package gateway

import (
	"context"
	"encoding/json"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"testing"
)

func TestCognitionPolicyEndpoint(t *testing.T) {
	g := &OpenAIGateway{config: cognitionConfig{enabled: true, prompt: "question", pattern: "^42$"}}
	status, _, body, err := g.HandleRequest(context.Background(), "GET", "accounts/cognition-test-policy", "", nil, nil)
	var policy struct {
		Enabled bool
		Prompt  string
		Regexp  string
	}
	if err != nil || status != 200 || json.Unmarshal(body, &policy) != nil || !policy.Enabled || policy.Prompt != "question" || policy.Regexp != "^42$" {
		t.Fatalf("policy=%s status=%d err=%v", body, status, err)
	}
	g.config = cognitionConfig{enabled: false, pattern: "["}
	status, _, body, err = g.HandleRequest(context.Background(), "GET", "accounts/cognition-test-policy", "", nil, nil)
	if err != nil || status != 200 || string(body) != "{\"enabled\":false}" {
		t.Fatalf("disabled policy=%s err=%v", body, err)
	}
}

type cognitionConfig struct {
	sdk.PluginConfig
	enabled         bool
	prompt, pattern string
}

func (c cognitionConfig) GetBool(string) bool { return c.enabled }
func (c cognitionConfig) GetString(key string) string {
	if key == "cognition_test_prompt" {
		return c.prompt
	}
	return c.pattern
}

func TestCognitionConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		cfg     cognitionConfig
		invalid bool
	}{
		{cognitionConfig{enabled: false, pattern: "["}, false},
		{cognitionConfig{enabled: true, prompt: "test", pattern: "^42$"}, false},
		{cognitionConfig{enabled: true, prompt: "test", pattern: "["}, true},
		{cognitionConfig{enabled: true, pattern: "ok"}, true},
		{cognitionConfig{enabled: true, prompt: "test"}, true},
	} {
		if err := validateCognitionTestConfig(tc.cfg); (err != nil) != tc.invalid {
			t.Fatalf("config=%+v error=%v", tc.cfg, err)
		}
	}
}
