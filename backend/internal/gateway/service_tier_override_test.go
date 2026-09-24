package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

var serviceTierOverrideCases = []struct {
	name, group, requested, reported, wire, billed string
	multiplier                                     float64
}{
	{"group-fast-default-client", "fast", "default", "", "priority", "priority", 2},
	{"group-priority-no-client", "priority", "", "", "priority", "priority", 2},
	{"group-priority-flex-client", "priority", "flex", "priority", "priority", "priority", 2},
	{"group-fast-upstream-downgrade", "fast", "default", "default", "priority", "", 1},
	{"group-default-priority-client", "default", "priority", "priority", "default", "", 1},
	{"group-flex-priority-client", "flex", "priority", "", "flex", "flex", 0.5},
	{"group-flex-upstream-default", "flex", "priority", "default", "flex", "", 1},
	{"group-flex-upstream-priority", "flex", "default", "priority", "flex", "priority", 2},
	{"normalized-group", " FAST ", "flex", "priority", "priority", "priority", 2},
	{"invalid-group-keeps-client", "invalid-tier", "fast", "", "priority", "priority", 2},
	{"empty-group-keeps-client", "", "priority", "", "priority", "priority", 2},
	{"empty-group-keeps-flex", "", "flex", "flex", "flex", "flex", 0.5},
	{"no-overrides", "", "", "priority", "default", "", 1},
}

func TestServiceTierGroupOverrideForwardingAndBilling(t *testing.T) {
	for _, protocol := range serviceTierTestProtocols {
		for _, tc := range serviceTierOverrideCases {
			t.Run(protocol.name+"/"+tc.name, func(t *testing.T) {
				outcome, wireBody := forwardServiceTierTest(t, protocol, tc.requested, tc.reported, "response.completed", "", tc.group)
				if outcome.Kind != sdk.OutcomeSuccess {
					t.Fatalf("outcome = %s: %s", outcome.Kind, outcome.Reason)
				}
				if got := gjson.GetBytes(wireBody, "service_tier").String(); got != tc.wire {
					t.Fatalf("upstream tier = %q, want group-resolved %q; body=%s", got, tc.wire, wireBody)
				}
				assertServiceTierTestUsage(t, outcome.Usage, tc.billed, tc.multiplier)
			})
		}
	}
}

func TestServiceTierGroupOverrideOAuthRequestBuilders(t *testing.T) {
	for _, shape := range []string{"responses", "chat", "response.create", "compact"} {
		for _, tc := range serviceTierOverrideCases {
			t.Run(shape+"/"+tc.name, func(t *testing.T) {
				payload := map[string]any{"model": "gpt-5.6-sol"}
				if shape == "chat" {
					payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
				} else {
					payload["input"] = []any{map[string]any{"role": "user", "content": "hello"}}
				}
				if shape == "response.create" {
					payload["type"] = "response.create"
				}
				if tc.requested != "" {
					payload["service_tier"] = tc.requested
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				req := &sdk.ForwardRequest{
					Account: &sdk.Account{Credentials: map[string]string{"access_token": "test-oauth"}},
					Body:    body,
					Model:   "gpt-5.6-sol",
					Headers: http.Header{"X-Airgate-Service-Tier": {tc.group}},
				}
				var wire []byte
				if shape == "compact" {
					wire = preprocessRequestBody(body, req.Model, "/v1/responses/compact", req.Headers)
				} else {
					wire, err = (&OpenAIGateway{}).buildWSRequest(req, openAISessionResolution{})
					if err != nil {
						t.Fatal(err)
					}
				}
				if got := gjson.GetBytes(wire, "service_tier").String(); got != tc.wire {
					t.Fatalf("OAuth %s tier = %q, want %q; body=%s", shape, got, tc.wire, wire)
				}
				// forwardOAuth derives its billing fallback from this same request
				// resolution, so it must match the tier sent after protocol conversion.
				effectiveTier := resolveOpenAIRequestServiceTier(req.Body, req.Headers)
				if effectiveTier != tc.wire {
					t.Fatalf("billing request tier = %q, wire tier = %q", effectiveTier, tc.wire)
				}
				usage := newTokenUsage(req.Model, resolveOpenAIUsageServiceTier(effectiveTier, tc.reported), 70, 50, 20, 10, 0, 0)
				fillUsageCost(usage)
				assertServiceTierTestUsage(t, usage, tc.billed, tc.multiplier)
			})
		}
	}
}
