package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"github.com/tidwall/gjson"
)

// Include all billing fields while excluding protocol-specific timing/metadata.
type unifiedBillSnapshot struct {
	Model, Currency, Tier                                     string
	Input, Output, Cached, Created, Reasoning                 int
	InputPrice, OutputPrice, CachedPrice, CreatedPrice        float64
	InputCost, OutputCost, CachedCost, CreatedCost, TotalCost float64
}

func snapshotUnifiedBill(t *testing.T, usage *sdk.Usage) unifiedBillSnapshot {
	t.Helper()
	if usage == nil {
		t.Fatal("missing billable usage")
	}
	return unifiedBillSnapshot{
		Model: usage.Model, Currency: usage.Currency, Tier: usageServiceTier(usage),
		Input: usage.InputTokens, Output: usage.OutputTokens, Cached: usage.CachedInputTokens, Created: usage.CacheCreationTokens, Reasoning: usageMetricInt(usage, usageMetricReasoningOutputTokens),
		InputPrice: usage.InputPrice, OutputPrice: usage.OutputPrice, CachedPrice: usage.CachedInputPrice, CreatedPrice: usage.CacheCreationPrice,
		InputCost: usage.InputCost, OutputCost: usage.OutputCost, CachedCost: usage.CachedInputCost, CreatedCost: usage.CacheCreationCost, TotalCost: usage.AccountCost,
	}
}

func forwardUnifiedBPSBillingTest(t *testing.T, protocol serviceTierTestProtocol, requested, reported, terminal, group string, reportedModel ...string) sdk.ForwardOutcome {
	t.Helper()
	req := bpsRequest()
	req.Stream = protocol.stream
	req.Writer = httptest.NewRecorder()
	req.Headers.Set("X-Forwarded-Path", protocol.path)
	req.Headers.Set("X-Airgate-Request-Path", protocol.path)
	req.Headers.Set("X-Forwarded-Method", http.MethodPost)
	req.Headers.Set("Content-Type", "application/json")
	req.Headers.Set("X-Airgate-Service-Tier", group)
	req.DispatchPlan = sdk.DispatchPlan{SchedulingModel: req.Model, WireModel: req.Model}
	payload := map[string]any{"model": req.Model, "stream": protocol.stream}
	if protocol.chat || protocol.anthropic {
		payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	} else {
		payload["input"] = "hello"
	}
	if protocol.anthropic {
		payload["model"] = "claude-sonnet-4-6"
		payload["max_tokens"] = 128
	}
	if requested != "" {
		payload["service_tier"] = requested
	}
	var err error
	req.Body, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	wire := serviceTierTestResponse(t, serviceTierTestProtocols[1], reported, terminal, "")
	if len(reportedModel) > 0 {
		wire = strings.ReplaceAll(wire, req.Model, reportedModel[0])
	}
	g := bpsGateway(t, req, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "service_tier").Exists() {
			t.Error("BAS must still omit the unsupported wire field")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	})
	outcome, err := g.forwardHTTP(context.Background(), req)
	if terminal == "response.completed" && (err != nil || outcome.Kind != sdk.OutcomeSuccess) {
		t.Fatalf("forwardHTTP: outcome=%+v err=%v", outcome, err)
	}
	return outcome
}

func TestUnifiedBillingIndependentOfTransportAndProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, request, reported, group, billed string
		multiplier                             float64
	}{
		{"unrequested-priority", "", "priority", "", "priority", 2},
		{"unrequested-flex", "", "flex", "", "flex", 0.5},
		{"ordinary", "", "default", "", "", 1},
		{"downgrade", "fast", "default", "", "", 1},
		{"flex-to-priority", "flex", "priority", "", "priority", 2},
		{"priority-to-flex", "priority", "flex", "", "flex", 0.5},
		{"missing-fallback", "fast", "", "", "priority", 2},
		{"auto-fallback", "flex", "auto", "", "flex", 0.5},
		{"unknown-fallback", "fast", "unknown", "", "priority", 2},
		{"missing-ordinary", "", "", "", "", 1},
		{"group-fallback", "default", "", "fast", "priority", 2},
		{"actual-over-group", "fast", "default", "priority", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reference unifiedBillSnapshot
			for i, protocol := range serviceTierTestProtocols {
				native, _ := forwardServiceTierTest(t, protocol, tc.request, tc.reported, "response.completed", "", tc.group)
				assertServiceTierTestUsage(t, native.Usage, tc.billed, tc.multiplier)
				bill := snapshotUnifiedBill(t, native.Usage)
				if i == 0 {
					reference = bill
				} else if !reflect.DeepEqual(reference, bill) {
					t.Errorf("native/%s bill differs: %+v want %+v", protocol.name, bill, reference)
				}
				if protocol.name == "compact-json" {
					continue
				}
				bps := forwardUnifiedBPSBillingTest(t, protocol, tc.request, tc.reported, "response.completed", tc.group)
				assertServiceTierTestUsage(t, bps.Usage, tc.billed, tc.multiplier)
				if bill := snapshotUnifiedBill(t, bps.Usage); !reflect.DeepEqual(reference, bill) {
					t.Errorf("BAS/%s bill differs: %+v want %+v", protocol.name, bill, reference)
				}
			}
		})
	}
}

func TestUnifiedBillingRetainedOnBASFailure(t *testing.T) {
	for _, terminal := range []string{"response.failed", "response.incomplete"} {
		for _, protocol := range serviceTierTestProtocols {
			if protocol.name == "compact-json" {
				continue
			}
			t.Run(terminal+"/"+protocol.name, func(t *testing.T) {
				outcome := forwardUnifiedBPSBillingTest(t, protocol, "default", "priority", terminal, "")
				assertServiceTierTestUsage(t, outcome.Usage, "priority", 2)
			})
		}
	}
}

func TestUnifiedBillingDefaultClearsEarlierPriority(t *testing.T) {
	usage := newTokenUsage("gpt-5.6-sol", "priority", 70, 50, 20, 10, 0, 0)
	fillUsageCost(usage)
	assertServiceTierTestUsage(t, usage, "priority", 2)
	setUsageServiceTier(usage, "default", "priority")
	fillUsageCost(usage)
	assertServiceTierTestUsage(t, usage, "", 1)
}

func TestUnifiedBillingUsesReportedModelAcrossProtocols(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.failed"} {
		var reference unifiedBillSnapshot
		for i, protocol := range serviceTierTestProtocols {
			if protocol.name == "compact-json" {
				continue
			}
			outcome := forwardUnifiedBPSBillingTest(t, protocol, "default", "priority", terminal, "", "gpt-5.4")
			bill := snapshotUnifiedBill(t, outcome.Usage)
			if bill.Model != "gpt-5.4" || bill.Tier != "priority" {
				t.Fatalf("%s/%s ignores upstream model/tier: %+v", terminal, protocol.name, bill)
			}
			if i == 0 {
				reference = bill
			} else if !reflect.DeepEqual(reference, bill) {
				t.Errorf("%s/%s: %+v want %+v", terminal, protocol.name, bill, reference)
			}
		}
	}
}
