package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type testTaskHandler struct {
	taskType string
}

func (h testTaskHandler) Type() string { return h.taskType }

func (h testTaskHandler) BuildInput(*sdk.ForwardRequest, string) (map[string]any, map[string]string, error) {
	return map[string]any{"ok": true}, map[string]string{"kind": "test"}, nil
}

func (h testTaskHandler) Execute(context.Context, *OpenAIGateway, sdk.HostTask, *TaskRuntime) error {
	return nil
}

func (h testTaskHandler) BuildResponse(task *sdk.HostTask) map[string]any {
	return map[string]any{"id": externalTaskID(task)}
}

func TestBuildPluginInfoAndRoutes(t *testing.T) {
	info := BuildPluginInfo()
	if info.ID != PluginID || info.Name != PluginDisplayName || info.Type != sdk.PluginTypeGateway {
		t.Fatalf("unexpected plugin info identity: %#v", info)
	}
	if info.Version != PluginVersion || info.SDKVersion != sdk.SDKVersion {
		t.Fatalf("unexpected version fields: %#v", info)
	}
	if len(info.Dependencies) != 0 || len(PluginDependencies()) != 0 {
		t.Fatal("OpenAI gateway should not declare plugin dependencies")
	}
	if len(info.AccountTypes) != 2 {
		t.Fatalf("AccountTypes len=%d, want 2", len(info.AccountTypes))
	}
	if len(info.FrontendWidgets) == 0 {
		t.Fatal("expected frontend widgets")
	}
	var adjustmentConfig *sdk.ConfigField
	for i := range info.ConfigSchema {
		if info.ConfigSchema[i].Key == accountPoolAdjustmentPlansConfigKey {
			adjustmentConfig = &info.ConfigSchema[i]
			break
		}
	}
	if adjustmentConfig == nil || adjustmentConfig.Type != "multiselect" || len(adjustmentConfig.Options) != 4 {
		t.Fatalf("unexpected account pool adjustment config: %#v", adjustmentConfig)
	}
	if !reflect.DeepEqual(info.InstructionPresets, []string{"default", "simple", "nsfw", "cc"}) {
		t.Fatalf("InstructionPresets = %#v", info.InstructionPresets)
	}
	if info.Metadata["account.oauth_plans"] == "" {
		t.Fatal("expected oauth plan metadata")
	}
	if info.Metadata["account_import.v1"] == "" {
		t.Fatal("expected account import capability metadata")
	}
	if !strings.Contains(info.Metadata["account_import.v1"], "refresh_token") {
		t.Fatal("expected refresh_token account import format")
	}
	if len(info.DispatchDSL.Rules) == 0 {
		t.Fatal("expected dispatch rules")
	}
	var compactRule *sdk.DispatchRule
	var responsesRule *sdk.DispatchRule
	for i := range info.DispatchDSL.Rules {
		switch info.DispatchDSL.Rules[i].ID {
		case "responses-compact":
			compactRule = &info.DispatchDSL.Rules[i]
		case "responses-default":
			responsesRule = &info.DispatchDSL.Rules[i]
		}
	}
	if compactRule == nil {
		t.Fatal("expected responses-compact dispatch rule")
	}
	if compactRule.Model.StripSuffix != compactModelSuffix {
		t.Fatalf("compact StripSuffix = %q, want %q", compactRule.Model.StripSuffix, compactModelSuffix)
	}
	if len(compactRule.Candidates) != 1 ||
		compactRule.Candidates[0].Scheduling != "${model.base}" ||
		compactRule.Candidates[0].Wire != "${model.base}" {
		t.Fatalf("compact candidates = %#v, want base scheduling and wire", compactRule.Candidates)
	}
	if responsesRule == nil {
		t.Fatal("expected responses-default dispatch rule")
	}
	if len(responsesRule.Candidates) != 1 ||
		responsesRule.Candidates[0].Scheduling != "${model}" ||
		responsesRule.Candidates[0].Wire != "${model}" {
		t.Fatalf("responses candidates = %#v, want identity only", responsesRule.Candidates)
	}

	caps := map[sdk.Capability]bool{}
	for _, cap := range info.Capabilities {
		caps[cap] = true
	}
	for _, want := range []sdk.Capability{
		sdk.CapabilityHostInvoke,
		sdk.CapabilityForHostMethod(hostMethodTasksCreate),
		sdk.CapabilityForHostMethod(hostMethodGatewayForward),
		sdk.CapabilityForHostMethod(hostMethodAssetsStoreURL),
	} {
		if !caps[want] {
			t.Fatalf("capability %q missing from %#v", want, info.Capabilities)
		}
	}

	routes := PluginRouteDefinitions()
	if len(routes) == 0 {
		t.Fatal("expected routes")
	}
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r.Method+" "+r.Path] = true
		if r.Description == "" {
			t.Fatalf("route %s %s missing description", r.Method, r.Path)
		}
	}
	for _, key := range []string{"POST /v1/responses", "GET /models", "WS /responses"} {
		if !seen[key] {
			t.Fatalf("route %q missing", key)
		}
	}
}

func TestLongContextModelConfiguration(t *testing.T) {
	t.Setenv(longContextModelEnv, "")
	if got := configuredLongContextModel(); got != defaultLongContextModel {
		t.Fatalf("configuredLongContextModel() = %q, want default %q", got, defaultLongContextModel)
	}

	t.Setenv(longContextModelEnv, "openai/gpt-long")
	if got := configuredLongContextModel(); got != "gpt-long" {
		t.Fatalf("configuredLongContextModel() = %q, want gpt-long", got)
	}

	t.Setenv(longContextModelEnv, "router@openai/gpt-next-long")
	if got := configuredLongContextModel(); got != "gpt-next-long" {
		t.Fatalf("configuredLongContextModel() = %q, want gpt-next-long", got)
	}

	t.Setenv(longContextModelEnv, "router@openai/")
	if got := configuredLongContextModel(); got != defaultLongContextModel {
		t.Fatalf("invalid long-context mapping = %q, want default %q", got, defaultLongContextModel)
	}
}

func TestGatewayMetadataMethods(t *testing.T) {
	g := &OpenAIGateway{}
	if got := g.Info().ID; got != PluginID {
		t.Fatalf("Info().ID = %q", got)
	}
	if got := g.Platform(); got != PluginPlatform {
		t.Fatalf("Platform() = %q", got)
	}
	if len(g.Models()) == 0 {
		t.Fatal("Models should return registry models")
	}
	if len(g.Routes()) == 0 {
		t.Fatal("Routes should return plugin routes")
	}
}

func TestGatewayLifecycleWithoutContext(t *testing.T) {
	g := &OpenAIGateway{}
	if err := g.Init(nil); err != nil {
		t.Fatalf("Init(nil) returned err: %v", err)
	}
	if g.logger == nil {
		t.Fatal("Init should install a default logger")
	}
	if g.transportPool == nil {
		t.Fatal("Init should create a transport pool")
	}
	if got := g.TaskTypes(); !reflect.DeepEqual(got, []string{"image.edit", "image.generate"}) {
		t.Fatalf("TaskTypes = %#v", got)
	}
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start returned err: %v", err)
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("Stop returned err: %v", err)
	}
}

func TestGatewayStopHandlesNilResources(t *testing.T) {
	g := &OpenAIGateway{logger: slog.Default()}
	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("Stop with nil resources returned err: %v", err)
	}
}

func TestTaskRegistry(t *testing.T) {
	if (*TaskRegistry)(nil).Get("missing") != nil {
		t.Fatal("nil registry Get should return nil")
	}
	if (*TaskRegistry)(nil).Types() != nil {
		t.Fatal("nil registry Types should return nil")
	}

	registry := NewTaskRegistry()
	registry.Register(testTaskHandler{taskType: "z.task"})
	registry.Register(testTaskHandler{taskType: "a.task"})
	if got := registry.Get("z.task"); got == nil || got.Type() != "z.task" {
		t.Fatalf("Get returned %#v", got)
	}
	if got := registry.Types(); !reflect.DeepEqual(got, []string{"a.task", "z.task"}) {
		t.Fatalf("Types = %#v", got)
	}
}

func TestTaskRunnerHelpers(t *testing.T) {
	if got := sanitizeTaskMessage(nil); got != "" {
		t.Fatalf("sanitizeTaskMessage(nil) = %q", got)
	}
	if got := sanitizeTaskMessage(&TaskError{Type: "rate_limited", Message: "rpc error: code = Unknown"}); got != "当前请求过多，请稍后重试" {
		t.Fatalf("grpc without desc fallback = %q", got)
	}
	if got := sanitizeTaskMessage(&TaskError{Type: "auth_error", Message: "upstream 转发失败: closed"}); got != "账号认证失败，请联系管理员" {
		t.Fatalf("upstream auth fallback = %q", got)
	}
	if got := extractGRPCDesc("prefix desc = detail "); got != "detail" {
		t.Fatalf("extractGRPCDesc = %q", got)
	}

	if got := imageTaskLocation("/images/generations", "task 1"); got != "/images/tasks?task_id=task+1" {
		t.Fatalf("imageTaskLocation root = %q", got)
	}
	if got := imageTaskLocation("/v1/images/generations", "task/1"); got != "/v1/images/tasks?task_id=task%2F1" {
		t.Fatalf("imageTaskLocation v1 = %q", got)
	}

	headers := http.Header{taskExecHeader: []string{"true"}}
	if !isTaskExecution(headers) {
		t.Fatal("task execution header should be detected")
	}
	headers.Set(taskExecHeader, "false")
	if isTaskExecution(headers) {
		t.Fatal("non-true task execution header should not be detected")
	}

	req := &sdk.ForwardRequest{Headers: http.Header{"X-Forwarded-Query": []string{"task_id= public-123 "}}}
	if got := imageTaskIDFromRequest(req); got != "public-123" {
		t.Fatalf("task ID from query = %q", got)
	}
	req = &sdk.ForwardRequest{Body: []byte(`{"task_id":123}`)}
	if got := imageTaskIDFromRequest(req); got != "123" {
		t.Fatalf("task ID from JSON number = %q", got)
	}
	req = &sdk.ForwardRequest{Body: []byte(`not json`)}
	if got := imageTaskIDFromRequest(req); got != "" {
		t.Fatalf("invalid body task ID = %q", got)
	}
	if got := imageTaskIDFromRequest(nil); got != "" {
		t.Fatalf("nil request task ID = %q", got)
	}

	if !isNumeric("123") || isNumeric("") || isNumeric("12x") {
		t.Fatal("isNumeric returned unexpected result")
	}
	if got := externalTaskID(&sdk.HostTask{ID: 12, PublicTaskID: "pub"}); got != "pub" {
		t.Fatalf("external public ID = %q", got)
	}
	if got := externalTaskID(&sdk.HostTask{ID: 12}); got != "12" {
		t.Fatalf("external numeric ID = %q", got)
	}
	if got := externalTaskID(nil); got != "" {
		t.Fatalf("external nil ID = %q", got)
	}

	if got := stringIDFromAny(" id "); got != "id" {
		t.Fatalf("stringIDFromAny string = %q", got)
	}
	if got := stringIDFromAny(float64(42)); got != "42" {
		t.Fatalf("stringIDFromAny float = %q", got)
	}
	if got := stringIDFromAny(json.Number("77")); got != "77" {
		t.Fatalf("stringIDFromAny json.Number = %q", got)
	}
	if got := stringIDFromAny(float64(1.5)); got != "" {
		t.Fatalf("fractional ID should be rejected, got %q", got)
	}
}
