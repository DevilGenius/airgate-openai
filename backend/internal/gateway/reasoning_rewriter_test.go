package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/DevilGenius/airgate-openai/backend/internal/basispoints"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestReasoningRewriteAtOAuthAndBASBoundaries(t *testing.T) {
	for _, tc := range []struct {
		model, requested, oauth, bas string
	}{
		{"gpt-5.6-sol", "max", "max", "xhigh"},
		{"gpt-5.6-sol", "ultra", "max", "xhigh"},
		{"gpt-6-astra", "max", "max", "xhigh"},
		{"gpt-6-astra", " ULTRA ", "max", "xhigh"},
		{"gpt-6-astra", "xhigh", "xhigh", "xhigh"},
		{"gpt-6-astra", "minimal", "none", "low"},
		{"gpt-6-sol", "max", "max", "xhigh"},
		{"gpt-6-sol", "ultra", "max", "xhigh"},
		{"gpt-6-luna", "max", "max", "xhigh"},
		{"gpt-6-luna", "ultra", "max", "xhigh"},
		{"gpt-6.1-sol", "max", "max", "xhigh"},
		{"gpt-6.1-sol", "ultra", "max", "xhigh"},
		{"gpt-5.5", "max", "xhigh", "xhigh"},
		{"gpt-5.5", "ultra", "xhigh", "xhigh"},
		{"unlisted-model", "ultra", "max", "xhigh"},
		{"unlisted-model", "max", "max", "xhigh"},
		{"unlisted-model", "high", "high", "high"},
	} {
		t.Run(tc.model+"/"+tc.requested, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"model": tc.model, "input": "hello", "reasoning": map[string]string{"effort": tc.requested}})
			if err != nil {
				t.Fatal(err)
			}
			original := bytes.Clone(body)
			httpBody, err := rewriteOpenAIReasoningEffort(body, tc.model)
			if err != nil || gjson.GetBytes(httpBody, "reasoning.effort").String() != tc.oauth {
				t.Fatalf("native HTTP effort: %s / %v", httpBody, err)
			}
			wsBody, err := (&OpenAIGateway{}).buildWSRequest(&sdk.ForwardRequest{Body: body, Model: tc.model}, openAISessionResolution{})
			if err != nil || gjson.GetBytes(wsBody, "reasoning.effort").String() != tc.oauth {
				t.Fatalf("native WS effort: %s / %v", wsBody, err)
			}
			prepared, err := basispoints.PrepareRequest(body, "effort-contract")
			if err != nil {
				t.Fatal(err)
			}
			basBody, err := io.ReadAll(prepared.Body())
			if err != nil || gjson.GetBytes(basBody, "reasoning_effort").String() != tc.bas {
				t.Fatalf("BAS effort: %s / %v", basBody, err)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("wire rewriting changed the original request")
			}
		})
	}
}

func TestReasoningFieldPrecedenceAcrossTransports(t *testing.T) {
	for _, tc := range []struct {
		name, fields, requested, oauth, bas string
	}{
		{"nested wins", `"reasoning":{"effort":" ULTRA "},"reasoning_effort":"low","output_config":{"effort":"high"}`, " ULTRA ", "max", "xhigh"},
		{"summary only", `"reasoning":{"summary":"auto"},"reasoning_effort":"high"`, "high", "high", "high"},
		{"blank nested", `"reasoning":{"effort":" "},"reasoning_effort":"max"`, "max", "max", "xhigh"},
		{"output fallback", `"reasoning":{},"reasoning_effort":" ","output_config":{"effort":"normal"}`, "normal", "medium", "medium"},
		{"minimal", `"reasoning":{"effort":"min"}`, "min", "none", "low"},
		{"none", `"reasoning_effort":"off"`, "off", "none", "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-6-astra","input":"hello",` + tc.fields + `}`)
			original := bytes.Clone(body)
			basBody, bridge, err := basispoints.Prepare(body, "effort-precedence", nil)
			if err != nil {
				t.Fatal(err)
			}
			if bridge.RequestedEffort != tc.requested || bridge.Effort != tc.bas || gjson.GetBytes(basBody, "reasoning_effort").String() != tc.bas {
				t.Fatalf("BAS effort: requested=%q wire=%q want=%q/%q", bridge.RequestedEffort, bridge.Effort, tc.requested, tc.bas)
			}
			nativeBody, err := rewriteOpenAIReasoningEffort(ensureResponsesDefaultsWithTier(body, ""), "gpt-6-astra")
			if err != nil || gjson.GetBytes(nativeBody, "reasoning.effort").String() != tc.oauth {
				t.Fatalf("native effort: %s / %v, want %q", nativeBody, err, tc.oauth)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("effort selection mutated the caller request")
			}
		})
	}
}

func TestReasoningUnknownValueDoesNotFallBackToOtherFields(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":"hello","reasoning":{"effort":"future-effort"},"reasoning_effort":"high"}`)
	if _, _, err := basispoints.Prepare(body, "unknown-effort", nil); err == nil {
		t.Fatal("BAS accepted an unknown requested effort")
	}
	nativeBody, err := rewriteOpenAIReasoningEffort(body, "gpt-6-astra")
	if err != nil || gjson.GetBytes(nativeBody, "reasoning.effort").String() != "future-effort" {
		t.Fatalf("native upstream validation value changed: %s / %v", nativeBody, err)
	}
}
