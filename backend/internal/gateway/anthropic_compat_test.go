package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestEstimateAnthropicInputTokensCountsSystemMessagesAndTools(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4-5","system":"You are concise.","messages":[{"role":"user","content":[{"type":"text","text":"hello world"},{"type":"tool_result","tool_use_id":"toolu_1","content":"搜索结果"}]}],"tools":[{"name":"grep","description":"Search files","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}],"max_tokens":64}`)

	got := estimateAnthropicInputTokens(body)
	if got <= 40 {
		t.Fatalf("estimateAnthropicInputTokens() = %d, want a non-trivial estimate", got)
	}
}

func TestConvertAnthropicRequestToResponsesMapsMessageSystemRoleToDeveloper(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":8,"messages":[{"role":"system","content":"Reply with exactly OK."},{"role":"user","content":"test"}]}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "xhigh")
	if strings.Contains(string(got), `"role":"system"`) {
		t.Fatalf("converted Responses body still contains system role: %s", got)
	}
	if role := gjson.GetBytes(got, "input.0.role").String(); role != "developer" {
		t.Fatalf("first input role = %q, want developer; body=%s", role, got)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "Reply with exactly OK." {
		t.Fatalf("developer text = %q; body=%s", text, got)
	}
	if role := gjson.GetBytes(got, "input.1.role").String(); role != "user" {
		t.Fatalf("second input role = %q, want user; body=%s", role, got)
	}
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "xhigh" {
		t.Fatalf("reasoning effort = %q, want xhigh; body=%s", effort, got)
	}
}

func TestConvertAnthropicRequestToResponsesMapsDisabledThinkingToNone(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"thinking":{"type":"disabled"},"output_config":{"effort":"xhigh"},"messages":[{"role":"user","content":"Reply exactly OK."}]}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "high")
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "none" {
		t.Fatalf("reasoning effort = %q, want none; body=%s", effort, got)
	}
}

func TestConvertAnthropicRequestToResponsesUsesOutputEffortWhenThinkingAdaptive(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":8,"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"},"messages":[{"role":"user","content":"Review this."}]}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "xhigh" {
		t.Fatalf("reasoning effort = %q, want xhigh; body=%s", effort, got)
	}
}

func TestConvertAnthropicRequestToResponsesToolResultArrayOutputIsString(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_read","name":"Read","input":{"file_path":"a.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_read","content":[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]}]}]}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	item := gjson.GetBytes(got, "input.1")
	if typ := item.Get("type").String(); typ != "function_call_output" {
		t.Fatalf("input.1.type = %q, want function_call_output; body=%s", typ, got)
	}
	output := item.Get("output")
	if output.Type != gjson.String {
		t.Fatalf("function_call_output.output type = %v, want string; output=%s body=%s", output.Type, output.Raw, got)
	}
	if output.String() != "alpha\n\nbeta" {
		t.Fatalf("function_call_output.output = %q, want joined text; body=%s", output.String(), got)
	}
}

func TestConvertAnthropicRequestToResponsesToolResultImagesBecomeUserImages(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_view","name":"View","input":{"path":"shot.png"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_view","content":[{"type":"text","text":"screenshot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}]}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	if output := gjson.GetBytes(got, "input.1.output").String(); output != "screenshot" {
		t.Fatalf("function_call_output.output = %q, want screenshot; body=%s", output, got)
	}
	imageMsg := gjson.GetBytes(got, "input.2")
	if typ := imageMsg.Get("type").String(); typ != "message" {
		t.Fatalf("input.2.type = %q, want message; body=%s", typ, got)
	}
	if role := imageMsg.Get("role").String(); role != "user" {
		t.Fatalf("image message role = %q, want user; body=%s", role, got)
	}
	if typ := imageMsg.Get("content.0.type").String(); typ != "input_image" {
		t.Fatalf("image content type = %q, want input_image; body=%s", typ, got)
	}
	if url := imageMsg.Get("content.0.image_url").String(); url != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image_url = %q; body=%s", url, got)
	}
}

func TestConvertAnthropicRequestToResponsesSkipsToolSearchMetaAndUndiscoveredDeferredTools(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search"},{"type":"custom","name":"DeferredReview","description":"Launch a deferred worker","custom":{"defer_loading":true},"defer_loading":true,"allowed_callers":["code_execution_20250825"],"input_examples":[{"prompt":"review"}],"cache_control":{"type":"ephemeral"},"input_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"prompt":{"type":"string"}}}},{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	if count := gjson.GetBytes(got, "tools.#").Int(); count != 1 {
		t.Fatalf("tools count = %d, want one non-deferred function; body=%s", count, got)
	}
	tool := gjson.GetBytes(got, "tools.0")
	if typ := tool.Get("type").String(); typ != "function" {
		t.Fatalf("tool type = %q, want function; body=%s", typ, got)
	}
	if name := tool.Get("name").String(); name != "Read" {
		t.Fatalf("tool name = %q, want Read; body=%s", name, got)
	}
	if choice := gjson.GetBytes(got, "tool_choice").String(); choice != "auto" {
		t.Fatalf("tool_choice = %q, want auto; body=%s", choice, got)
	}
}

func TestConvertAnthropicRequestToResponsesIncludesDiscoveredDeferredToolAndSanitizesFields(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_search","content":[{"type":"tool_reference","tool_name":"DeferredReview"}]}]},{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"DeferredReview","description":"Launch a deferred worker","custom":{"defer_loading":true},"defer_loading":true,"allowed_callers":["code_execution_20250825"],"input_examples":[{"prompt":"review"}],"cache_control":{"type":"ephemeral"},"input_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"prompt":{"type":"string"}}}},{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	if count := gjson.GetBytes(got, "tools.#").Int(); count != 2 {
		t.Fatalf("tools count = %d, want discovered deferred function and regular function; body=%s", count, got)
	}
	tool := gjson.GetBytes(got, "tools.0")
	if typ := tool.Get("type").String(); typ != "function" {
		t.Fatalf("tool type = %q, want function; body=%s", typ, got)
	}
	if name := tool.Get("name").String(); name != "DeferredReview" {
		t.Fatalf("tool name = %q, want DeferredReview; body=%s", name, got)
	}
	if desc := tool.Get("description").String(); desc != "Launch a deferred worker" {
		t.Fatalf("tool description = %q; body=%s", desc, got)
	}
	if params := tool.Get("parameters"); params.Get("properties.prompt.type").String() != "string" {
		t.Fatalf("parameters not preserved: %s; body=%s", params.Raw, got)
	}
	if strings.Contains(tool.Get("parameters").Raw, "$schema") {
		t.Fatalf("parameters still contains $schema: %s; body=%s", tool.Get("parameters").Raw, got)
	}
	for _, field := range []string{"custom", "defer_loading", "allowed_callers", "input_examples", "cache_control", "input_schema"} {
		if tool.Get(field).Exists() {
			t.Fatalf("tool still contains Anthropic field %q: %s", field, got)
		}
	}
}

func TestConvertAnthropicRequestToResponsesSanitizesRegularToolFields(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"Lookup","description":"Look up project data","custom":{"note":"metadata"},"input_examples":[{"query":"status"}],"cache_control":{"type":"ephemeral"},"input_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"query":{"type":"string"}}}}]}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	tool := gjson.GetBytes(got, "tools.0")
	if typ := tool.Get("type").String(); typ != "function" {
		t.Fatalf("tool type = %q, want function; body=%s", typ, got)
	}
	if name := tool.Get("name").String(); name != "Lookup" {
		t.Fatalf("tool name = %q, want Lookup; body=%s", name, got)
	}
	if strict := tool.Get("strict"); !strict.Exists() || strict.Bool() {
		t.Fatalf("tool strict = %s, want explicit false; body=%s", strict.Raw, got)
	}
	for _, field := range []string{"custom", "defer_loading", "allowed_callers", "input_examples", "cache_control", "input_schema"} {
		if tool.Get(field).Exists() {
			t.Fatalf("tool still contains %q: %s; body=%s", field, tool.Raw, got)
		}
	}
	if choice := gjson.GetBytes(got, "tool_choice").String(); choice != "auto" {
		t.Fatalf("tool_choice = %q, want auto; body=%s", choice, got)
	}
}

func TestConvertAnthropicRequestToResponsesDowngradesMissingToolChoice(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}],"tool_choice":{"type":"tool","name":"Missing"}}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	if choice := gjson.GetBytes(got, "tool_choice").String(); choice != "auto" {
		t.Fatalf("tool_choice = %q, want auto; body=%s", choice, got)
	}
}

func TestConvertAnthropicRequestToResponsesPreservesDeferredToolChoice(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"DeferredReview","custom":{"defer_loading":true},"input_schema":{"type":"object","properties":{"prompt":{"type":"string"}}}}],"tool_choice":{"type":"tool","name":"DeferredReview"}}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	if count := gjson.GetBytes(got, "tools.#").Int(); count != 1 {
		t.Fatalf("tools count = %d, want one sanitized function; body=%s", count, got)
	}
	choice := gjson.GetBytes(got, "tool_choice")
	if typ := choice.Get("type").String(); typ != "function" {
		t.Fatalf("tool_choice.type = %q, want function; body=%s", typ, got)
	}
	if name := choice.Get("name").String(); name != "DeferredReview" {
		t.Fatalf("tool_choice.name = %q, want DeferredReview; body=%s", name, got)
	}
}

func TestConvertAnthropicRequestToResponsesPreservesExistingToolChoice(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}],"tool_choice":{"type":"tool","name":"Read"}}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	choice := gjson.GetBytes(got, "tool_choice")
	if typ := choice.Get("type").String(); typ != "function" {
		t.Fatalf("tool_choice.type = %q, want function; body=%s", typ, got)
	}
	if name := choice.Get("name").String(); name != "Read" {
		t.Fatalf("tool_choice.name = %q, want Read; body=%s", name, got)
	}
}

func TestConvertAnthropicRequestToResponsesDowngradesAnyWhenOnlyToolSearchSkipped(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search"}],"tool_choice":{"type":"any"}}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	if count := gjson.GetBytes(got, "tools.#").Int(); count != 0 {
		t.Fatalf("tools count = %d, want 0; body=%s", count, got)
	}
	if choice := gjson.GetBytes(got, "tool_choice").String(); choice != "auto" {
		t.Fatalf("tool_choice = %q, want auto; body=%s", choice, got)
	}
}

func TestFinalizeAnthropicResponsesBodyDropsToolFieldsWithoutTools(t *testing.T) {
	body := []byte(`{"parallel_tool_calls":true,"tool_choice":"auto","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	got := finalizeAnthropicResponsesBody(body, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), "")
	if gjson.GetBytes(got, "parallel_tool_calls").Exists() {
		t.Fatalf("parallel_tool_calls should be removed without tools, body=%s", got)
	}
	if gjson.GetBytes(got, "tool_choice").Exists() {
		t.Fatalf("tool_choice should be removed without tools, body=%s", got)
	}
}

func TestConvertAnthropicRequestToResponsesDefaultsToolChoiceAutoWithTools(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}]}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	if choice := gjson.GetBytes(got, "tool_choice").String(); choice != "auto" {
		t.Fatalf("tool_choice = %q, want auto; body=%s", choice, got)
	}
}

func TestConvertAnthropicRequestToResponsesHonorsDisableParallelToolUse(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`)

	got := convertAnthropicRequestToResponses(body, "gpt-5.5", "medium")
	if parallel := gjson.GetBytes(got, "parallel_tool_calls"); !parallel.Exists() || parallel.Bool() {
		t.Fatalf("parallel_tool_calls = %s, want false; body=%s", parallel.Raw, got)
	}
}

func TestForwardAnthropicMessageUsesDispatchPlanModel(t *testing.T) {
	var models []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		model := gjson.GetBytes(body, "model").String()
		models = append(models, model)
		if model != sparkTargetModel {
			t.Fatalf("upstream model = %q, want Spark %q", model, sparkTargetModel)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"ok"}`+"\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp_dispatch_plan","model":"`+sparkTargetModel+`","usage":{"input_tokens":3,"output_tokens":1}}}`+"\n\n")
	}))
	defer ts.Close()

	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":128,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Grep","input":{"pattern":"foo"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"foo.go:1"}]}]}`)
	w := httptest.NewRecorder()
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: time.Now().UnixNano(), Credentials: map[string]string{
			"api_key":  "test-key",
			"base_url": ts.URL,
		}},
		Writer:       w,
		Body:         body,
		DispatchPlan: sdk.DispatchPlan{SchedulingModel: sparkTargetModel, WireModel: sparkTargetModel},
	}
	gateway := &OpenAIGateway{transportPool: NewTransportPool()}
	outcome, err := gateway.forwardAnthropicMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("forwardAnthropicMessage err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success; reason=%s", outcome.Kind, outcome.Reason)
	}
	if len(models) != 1 {
		t.Fatalf("upstream request count = %d, want 1; models=%v", len(models), models)
	}
	if got := gjson.Get(w.Body.String(), "content.0.text").String(); got != "ok" {
		t.Fatalf("response text = %q, want ok; body=%s", got, w.Body.String())
	}
}

func TestForwardAnthropicMessageNonStreamReturnsBodyForGRPC(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"pong"}`+"\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp_grpc","model":"gpt-5.4","usage":{"input_tokens":3,"output_tokens":1}}}`+"\n\n")
	}))
	defer ts.Close()

	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: time.Now().UnixNano(), Credentials: map[string]string{
			"api_key":  "test-key",
			"base_url": ts.URL,
		}},
		Body:         []byte(`{"model":"claude-sonnet-4-6","max_tokens":128,"messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`),
		DispatchPlan: sdk.DispatchPlan{SchedulingModel: sonnetTargetModel, WireModel: sonnetTargetModel},
	}

	gateway := &OpenAIGateway{transportPool: NewTransportPool()}
	outcome, err := gateway.forwardAnthropicMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("forwardAnthropicMessage err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success; reason=%s", outcome.Kind, outcome.Reason)
	}
	if len(outcome.Upstream.Body) == 0 {
		t.Fatalf("expected non-empty upstream body for non-stream gRPC path")
	}
	if got := outcome.Upstream.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "content.0.text").String(); got != "pong" {
		t.Fatalf("response text = %q, want pong; body=%s", got, outcome.Upstream.Body)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "model").String(); got != "claude-sonnet-4-6" {
		t.Fatalf("model = %q, want claude-sonnet-4-6", got)
	}
}

func TestExplicitAnthropicRequestServiceTier(t *testing.T) {
	req := &sdk.ForwardRequest{
		Headers: http.Header{"X-Airgate-Service-Tier": []string{"priority"}},
		Body:    []byte(`{"service_tier":"flex"}`),
	}
	if got := explicitAnthropicRequestServiceTier(req); got != "priority" {
		t.Fatalf("显式服务档位 = %q，期望 priority", got)
	}

	req.Headers = http.Header{}
	if got := explicitAnthropicRequestServiceTier(req); got != "flex" {
		t.Fatalf("请求体服务档位 = %q，期望 flex", got)
	}
}

func TestDefaultAnthropicUsageServiceTierAlwaysEmpty(t *testing.T) {
	oauthReq := &sdk.ForwardRequest{
		Account: &sdk.Account{Credentials: map[string]string{"access_token": "token"}},
	}
	if got := defaultAnthropicUsageServiceTier(oauthReq); got != "" {
		t.Fatalf("OAuth 默认服务档位 = %q，期望空值", got)
	}

	apiKeyReq := &sdk.ForwardRequest{
		Account: &sdk.Account{Credentials: map[string]string{"api_key": "sk-test"}},
	}
	if got := defaultAnthropicUsageServiceTier(apiKeyReq); got != "" {
		t.Fatalf("API Key 默认服务档位 = %q，期望空值", got)
	}
}

func TestHandleAnthropicNonStreamRecordsDefaultPriority(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"pong"}`,
		`data: {"type":"response.completed","response":{"id":"resp_priority","model":"gpt-5.4","usage":{"input_tokens":10,"output_tokens":2}}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}

	outcome, err := (&OpenAIGateway{}).handleAnthropicNonStreamFromResponses(
		resp,
		nil,
		"claude-sonnet-4-6",
		"gpt-5.4",
		[]byte(`{"model":"claude-sonnet-4-6","max_tokens":128,"messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`),
		"",
		"priority",
		time.Now(),
		openAISessionResolution{},
		0,
	)
	if err != nil {
		t.Fatalf("非流式回译失败: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("判决类型 = %v，期望成功；原因=%s", outcome.Kind, outcome.Reason)
	}
	if got := usageServiceTier(outcome.Usage); got != "priority" {
		t.Fatalf("usage service_tier = %q，期望 priority", got)
	}
}

func TestTranslateResponsesSSERecordsDefaultPriority(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"pong"}`,
		`data: {"type":"response.completed","response":{"id":"resp_priority","model":"gpt-5.4","usage":{"input_tokens":10,"output_tokens":2}}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	w := httptest.NewRecorder()

	outcome, err := translateResponsesSSEToAnthropicSSE(
		context.Background(),
		resp,
		w,
		"claude-sonnet-4-6",
		"gpt-5.4",
		[]byte(`{"model":"claude-sonnet-4-6","stream":true,"max_tokens":128,"messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`),
		"",
		"priority",
		time.Now(),
		openAISessionResolution{},
	)
	if err != nil {
		t.Fatalf("流式回译失败: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("判决类型 = %v，期望成功；原因=%s", outcome.Kind, outcome.Reason)
	}
	if got := usageServiceTier(outcome.Usage); got != "priority" {
		t.Fatalf("usage service_tier = %q，期望 priority", got)
	}
}

func TestForwardAnthropicCountTokensReturnsEstimate(t *testing.T) {
	w := httptest.NewRecorder()
	req := &sdk.ForwardRequest{
		Writer:  w,
		Headers: http.Header{"X-Forwarded-Path": []string{"/v1/messages/count_tokens"}},
		Body:    []byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hello world"}]}`),
	}
	outcome, err := (&OpenAIGateway{}).forwardAnthropicCountTokens(context.Background(), req)
	if err != nil {
		t.Fatalf("forwardAnthropicCountTokens err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success", outcome.Kind)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := gjson.Get(w.Body.String(), "input_tokens").Int(); got <= 0 {
		t.Fatalf("input_tokens = %d, want > 0; body=%s", got, w.Body.String())
	}
}

func TestNormalizeAnthropicStopReason(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty default", in: "", want: "end_turn"},
		{name: "stop to end_turn", in: "stop", want: "end_turn"},
		{name: "length to max_tokens", in: "length", want: "max_tokens"},
		{name: "tool_calls to tool_use", in: "tool_calls", want: "tool_use"},
		{name: "max_output_tokens to max_tokens", in: "max_output_tokens", want: "max_tokens"},
		{name: "content_filter to refusal", in: "content_filter", want: "refusal"},
		{name: "preserve unknown normalized", in: "  CUSTOM_REASON  ", want: "custom_reason"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeAnthropicStopReason(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeAnthropicStopReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSSEStream_AggregatesReasoningFunctionToolUseAndStopReason(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"think-"}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"step"}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Wuhan\"}"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","stop_reason":"tool_calls","usage":{"input_tokens":12,"output_tokens":34,"input_tokens_details":{"cached_tokens":5}}}}`,
		"",
	}, "\n")

	result := ParseSSEStream(strings.NewReader(sse), nil)
	if result.Err != nil {
		t.Fatalf("ParseSSEStream returned err: %v", result.Err)
	}
	if result.ResponseID != "resp_1" {
		t.Fatalf("ResponseID = %q, want %q", result.ResponseID, "resp_1")
	}
	if result.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want %q", result.Model, "gpt-5.4")
	}
	if result.Text != "hello" {
		t.Fatalf("Text = %q, want %q", result.Text, "hello")
	}
	if result.Reasoning != "think-step" {
		t.Fatalf("Reasoning = %q, want %q", result.Reasoning, "think-step")
	}
	if result.StopReason != "tool_calls" {
		t.Fatalf("StopReason = %q, want %q", result.StopReason, "tool_calls")
	}
	if result.InputTokens != 7 || result.OutputTokens != 34 || result.CachedInputTokens != 5 {
		t.Fatalf("usage = (%d,%d,%d), want (7,34,5)", result.InputTokens, result.OutputTokens, result.CachedInputTokens)
	}
	if len(result.ToolUses) != 1 {
		t.Fatalf("ToolUses len = %d, want 1", len(result.ToolUses))
	}
	if result.ToolUses[0].Type != "tool_use" || result.ToolUses[0].ID != "call_1" {
		t.Fatalf("unexpected tool_use block: %+v", result.ToolUses[0])
	}
	if result.ToolUses[0].Name == nil || *result.ToolUses[0].Name != "get_weather" {
		t.Fatalf("tool_use name = %v, want get_weather", result.ToolUses[0].Name)
	}
	if string(result.ToolUses[0].Input) != `{"city":"Wuhan"}` {
		t.Fatalf("tool_use input = %s, want %s", string(result.ToolUses[0].Input), `{"city":"Wuhan"}`)
	}
}

func TestParseSSEStream_AggregatesWebSearchToolUse(t *testing.T) {
	itemID := fmt.Sprintf("ws_%d", time.Now().UnixNano())
	query := fmt.Sprintf("weather-%d", time.Now().UnixNano())

	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_2"}}`,
		fmt.Sprintf(`data: {"type":"response.output_item.added","item":{"type":"web_search_call","id":%q}}`, itemID),
		fmt.Sprintf(`data: {"type":"response.output_item.done","item":{"type":"web_search_call","id":%q,"action":{"query":%q}}}`, itemID, query),
		`data: {"type":"response.completed","response":{"id":"resp_2","model":"gpt-5.4","stop_reason":"stop","usage":{"input_tokens":2,"output_tokens":3}}}`,
		"",
	}, "\n")

	result := ParseSSEStream(strings.NewReader(sse), nil)
	if result.Err != nil {
		t.Fatalf("ParseSSEStream returned err: %v", result.Err)
	}
	if len(result.ToolUses) != 1 {
		t.Fatalf("ToolUses len = %d, want 1", len(result.ToolUses))
	}
	tool := result.ToolUses[0]
	if tool.Name == nil || *tool.Name != "web_search" {
		t.Fatalf("websearch tool name = %v, want web_search", tool.Name)
	}
	if tool.ID != itemID {
		t.Fatalf("websearch tool id = %q, want %q", tool.ID, itemID)
	}
	wantInput := fmt.Sprintf(`{"query":%q}`, query)
	if string(tool.Input) != wantInput {
		t.Fatalf("websearch input = %s, want %s", string(tool.Input), wantInput)
	}
}

// 上游只通过 delta 下发文本、response.completed 里 output 为空的场景（真实 ChatGPT WebSocket 会话行为）
// 回退必须把 wsResult.Text 补到非流式响应的 content 数组里，否则客户端拿到空 content。
func TestConvertResponsesCompletedToAnthropicJSON_FallbackFromDeltas(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_abc123"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"think-"}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"step"}`,
		`data: {"type":"response.output_text.delta","delta":"你好"}`,
		`data: {"type":"response.output_text.delta","delta":"，世界"}`,
		`data: {"type":"response.completed","response":{"id":"resp_abc123","model":"gpt-5.4","usage":{"input_tokens":7,"output_tokens":13}}}`,
		"",
	}, "\n")

	result := ParseSSEStream(strings.NewReader(sse), nil)
	if result.Err != nil {
		t.Fatalf("ParseSSEStream err: %v", result.Err)
	}
	if result.Text != "你好，世界" {
		t.Fatalf("aggregated text = %q, want %q", result.Text, "你好，世界")
	}

	jsonOut := convertResponsesCompletedToAnthropicJSON(
		result.CompletedEventRaw,
		nil,
		"claude-sonnet-4-6",
		&result,
	)
	if jsonOut == "" {
		t.Fatalf("convertResponsesCompletedToAnthropicJSON returned empty")
	}

	// id 前缀必须从 resp_ 规范化为 msg_
	if id := gjson.Get(jsonOut, "id").String(); id != "msg_abc123" {
		t.Fatalf("id = %q, want msg_abc123", id)
	}
	if model := gjson.Get(jsonOut, "model").String(); model != "claude-sonnet-4-6" {
		t.Fatalf("model = %q, want claude-sonnet-4-6", model)
	}
	if sr := gjson.Get(jsonOut, "stop_reason").String(); sr != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", sr)
	}

	// content 必须非空，并且应包含 thinking + text 两个块
	contentLen := gjson.Get(jsonOut, "content.#").Int()
	if contentLen < 2 {
		t.Fatalf("content length = %d, want >= 2, full=%s", contentLen, jsonOut)
	}
	if got := gjson.Get(jsonOut, "content.0.type").String(); got != "thinking" {
		t.Fatalf("content[0].type = %q, want thinking", got)
	}
	if got := gjson.Get(jsonOut, "content.0.thinking").String(); got != "think-step" {
		t.Fatalf("content[0].thinking = %q, want think-step", got)
	}
	if got := gjson.Get(jsonOut, "content.1.type").String(); got != "text" {
		t.Fatalf("content[1].type = %q, want text", got)
	}
	if got := gjson.Get(jsonOut, "content.1.text").String(); got != "你好，世界" {
		t.Fatalf("content[1].text = %q, want %q", got, "你好，世界")
	}

	// usage 字段要带齐 4 个 token 字段
	if inp := gjson.Get(jsonOut, "usage.input_tokens").Int(); inp != 7 {
		t.Fatalf("usage.input_tokens = %d, want 7", inp)
	}
	if out := gjson.Get(jsonOut, "usage.output_tokens").Int(); out != 13 {
		t.Fatalf("usage.output_tokens = %d, want 13", out)
	}
}

func TestEnsureAnthropicStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "end_turn",
		"max_tokens":    "max_tokens",
		"stop_sequence": "stop_sequence",
		"tool_use":      "tool_use",
		"refusal":       "refusal",
		"pause_turn":    "pause_turn",
		// 非法值统一降级为 end_turn
		"":               "end_turn",
		"some_garbage":   "end_turn",
		"content_filter": "end_turn", // 未经 normalize 的原始值不在白名单里
	}
	for in, want := range cases {
		if got := ensureAnthropicStopReason(in); got != want {
			t.Errorf("ensureAnthropicStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenerateAnthropicRequestID_Format(t *testing.T) {
	for i := 0; i < 5; i++ {
		id := generateAnthropicRequestID()
		if !strings.HasPrefix(id, "req_01") {
			t.Fatalf("request id %q missing req_01 prefix", id)
		}
		// req_01 + 32 字符 hex = 38 字符
		if len(id) != 38 {
			t.Fatalf("request id %q length = %d, want 38", id, len(id))
		}
	}
}

func TestGenerateCloudflareRay_Format(t *testing.T) {
	for i := 0; i < 5; i++ {
		ray := generateCloudflareRay()
		if !strings.HasSuffix(ray, "-SJC") {
			t.Fatalf("cf-ray %q missing -SJC suffix", ray)
		}
		// 16 字符 hex + "-SJC" = 20 字符
		if len(ray) != 20 {
			t.Fatalf("cf-ray %q length = %d, want 20", ray, len(ray))
		}
	}
}

// 验证流式 message_start 事件后紧跟 ping 事件，对齐 Claude 官方行为
func TestConvertResponsesEventToAnthropic_MessageStartEmitsPing(t *testing.T) {
	state := &anthropicStreamState{}
	line := []byte(`data: {"type":"response.created","response":{"id":"resp_xyz","model":"gpt-5.4"}}`)
	out := convertResponsesEventToAnthropic(line, nil, state, "claude-sonnet-4-6")

	if !strings.Contains(out, "event: message_start") {
		t.Fatalf("missing message_start event, got: %s", out)
	}
	if !strings.Contains(out, "event: ping") {
		t.Fatalf("missing ping event, got: %s", out)
	}
	if !strings.Contains(out, `"type":"ping"`) {
		t.Fatalf("ping event payload wrong, got: %s", out)
	}
	// message_start 必须在 ping 前面
	msi := strings.Index(out, "message_start")
	pi := strings.Index(out, "ping")
	if msi < 0 || pi < 0 || msi > pi {
		t.Fatalf("message_start must come before ping, got: %s", out)
	}
	// id 前缀规范化
	if !strings.Contains(out, `"id":"msg_xyz"`) {
		t.Fatalf("message id not normalized to msg_ prefix, got: %s", out)
	}
	// usage 必须包含 Claude Code usage 累加器要求的完整字段：
	// - service_tier 字段（原生 Anthropic 下发）
	// - cache_creation 嵌套对象（Mo$ 合并函数直接访问 .ephemeral_1h_input_tokens，缺失会崩）
	if !strings.Contains(out, `"service_tier":"standard"`) {
		t.Fatalf("message_start usage missing service_tier, got: %s", out)
	}
	if !strings.Contains(out, `"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}`) {
		t.Fatalf("message_start usage missing cache_creation nested object, got: %s", out)
	}
	// 但绝不能包含 server_tool_use: null —— 会触发 JS `||` 短路转 undefined 后访问 .input_tokens 崩溃
	if strings.Contains(out, `"server_tool_use":null`) {
		t.Fatalf("message_start usage must NOT contain server_tool_use:null (triggers SDK undefined.input_tokens), got: %s", out)
	}
}

func TestConvertResponsesEventToAnthropicFunctionCallLifecycleDefersStopForBatching(t *testing.T) {
	state := &anthropicStreamState{}
	request := []byte(`{"tools":[{"name":"Skill","input_schema":{"type":"object","properties":{"skill":{"type":"string"}}}}]}`)

	added := []byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"Skill","arguments":""}}`)
	out := convertResponsesEventToAnthropic(added, request, state, "claude-sonnet-4-6")
	if !strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("missing tool_use start, got: %s", out)
	}
	if strings.Contains(out, `"input_json_delta"`) {
		t.Fatalf("tool_use start should not emit an empty input_json_delta, got: %s", out)
	}
	if strings.Contains(out, "event: content_block_stop") {
		t.Fatalf("tool_use should stay open before response completion, got: %s", out)
	}

	argsDone := []byte(`data: {"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"skill\":\"claude-api\"}"}`)
	out = convertResponsesEventToAnthropic(argsDone, request, state, "claude-sonnet-4-6")
	if !strings.Contains(out, `"partial_json":"{\"skill\":\"claude-api\"}"`) {
		t.Fatalf("missing full arguments delta, got: %s", out)
	}
	if strings.Contains(out, "event: content_block_stop") {
		t.Fatalf("arguments done should defer tool_use close for same-turn batching, got: %s", out)
	}

	itemDone := []byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"Skill","arguments":"{\"skill\":\"claude-api\"}"}}`)
	if out := convertResponsesEventToAnthropic(itemDone, request, state, "claude-sonnet-4-6"); out != "" {
		t.Fatalf("output_item.done after arguments done should be a no-op, got: %s", out)
	}

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":1,"output_tokens":2}}}`)
	out = convertResponsesEventToAnthropic(completed, request, state, "claude-sonnet-4-6")
	if !strings.Contains(out, "event: content_block_stop") {
		t.Fatalf("response completed should close deferred tool_use block, got: %s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("message_delta should report tool_use stop, got: %s", out)
	}
}

func TestConvertResponsesEventToAnthropicFunctionCallDeltaDefersStopUntilCompleted(t *testing.T) {
	state := &anthropicStreamState{}
	request := []byte(`{"tools":[{"name":"Edit","input_schema":{"type":"object","properties":{"file":{"type":"string"}}}}]}`)

	_ = convertResponsesEventToAnthropic([]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_2","name":"Edit","arguments":""}}`), request, state, "claude-sonnet-4-6")
	out := convertResponsesEventToAnthropic([]byte(`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"file\""}`), request, state, "claude-sonnet-4-6")
	if !strings.Contains(out, `"partial_json":"{\"file\""`) {
		t.Fatalf("missing streamed arguments delta, got: %s", out)
	}

	out = convertResponsesEventToAnthropic([]byte(`data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"file\":\"a.go\"}"}`), request, state, "claude-sonnet-4-6")
	if strings.Contains(out, `"partial_json":"{\"file\":\"a.go\"}"`) {
		t.Fatalf("arguments done should not resend full args after deltas, got: %s", out)
	}
	if strings.Contains(out, "event: content_block_stop") {
		t.Fatalf("arguments done should defer streamed tool_use close, got: %s", out)
	}

	out = convertResponsesEventToAnthropic([]byte(`data: {"type":"response.completed","response":{"id":"resp_2","usage":{"input_tokens":1,"output_tokens":2}}}`), request, state, "claude-sonnet-4-6")
	if !strings.Contains(out, "event: content_block_stop") {
		t.Fatalf("response completed should close streamed tool_use block, got: %s", out)
	}
}

func TestConvertResponsesEventToAnthropicFunctionCallDeltasRouteByOutputIndex(t *testing.T) {
	state := &anthropicStreamState{}
	request := []byte(`{"tools":[{"name":"First","input_schema":{"type":"object"}},{"name":"Second","input_schema":{"type":"object"}}]}`)

	out := convertResponsesEventToAnthropic([]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"First","arguments":""}}`), request, state, "claude-sonnet-4-6")
	if !strings.Contains(out, `"index":0`) || strings.Contains(out, "event: content_block_stop") {
		t.Fatalf("first tool should open index 0 without closing, got: %s", out)
	}

	out = convertResponsesEventToAnthropic([]byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_2","name":"Second","arguments":""}}`), request, state, "claude-sonnet-4-6")
	if !strings.Contains(out, `"index":1`) || strings.Contains(out, "event: content_block_stop") {
		t.Fatalf("second tool should open index 1 without closing first, got: %s", out)
	}

	out = convertResponsesEventToAnthropic([]byte(`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"a\""}`), request, state, "claude-sonnet-4-6")
	if !strings.Contains(out, `"index":0`) || !strings.Contains(out, `"partial_json":"{\"a\""`) {
		t.Fatalf("first tool delta should route to index 0, got: %s", out)
	}

	out = convertResponsesEventToAnthropic([]byte(`data: {"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"b\":2}"}`), request, state, "claude-sonnet-4-6")
	if !strings.Contains(out, `"index":1`) || !strings.Contains(out, `"partial_json":"{\"b\":2}"`) || strings.Contains(out, "event: content_block_stop") {
		t.Fatalf("second tool done should emit args without closing index 1, got: %s", out)
	}

	out = convertResponsesEventToAnthropic([]byte(`data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"a\":1}"}`), request, state, "claude-sonnet-4-6")
	if strings.Contains(out, `"partial_json":"{\"a\":1}"`) || strings.Contains(out, "event: content_block_stop") {
		t.Fatalf("first tool done should not resend or close already-streamed index 0, got: %s", out)
	}

	out = convertResponsesEventToAnthropic([]byte(`data: {"type":"response.completed","response":{"id":"resp_3","usage":{"input_tokens":1,"output_tokens":2}}}`), request, state, "claude-sonnet-4-6")
	if strings.Count(out, "event: content_block_stop") != 2 {
		t.Fatalf("response completed should close both deferred tool blocks, got: %s", out)
	}
	if strings.Index(out, `"index":0`) > strings.Index(out, `"index":1`) {
		t.Fatalf("deferred tool blocks should close in block index order, got: %s", out)
	}
}

func TestNormalizeAnthropicMessageID(t *testing.T) {
	cases := map[string]string{
		"":                              "",
		"resp_abc123":                   "msg_abc123",
		"msg_xyz":                       "msg_xyz",
		"  resp_trim  ":                 "msg_trim",
		"resp_0a530ec6a62d78460169df00": "msg_0a530ec6a62d78460169df00",
		"unknown_prefix_99":             "msg_unknown_prefix_99",
	}
	for in, want := range cases {
		if got := normalizeAnthropicMessageID(in); got != want {
			t.Errorf("normalizeAnthropicMessageID(%q) = %q, want %q", in, got, want)
		}
	}
}
