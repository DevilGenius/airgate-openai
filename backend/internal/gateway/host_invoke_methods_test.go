package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type invokeFakeHost struct {
	calls  []sdk.HostInvokeRequest
	invoke func(context.Context, sdk.HostInvokeRequest) (*sdk.HostInvokeResponse, error)
}

func (h *invokeFakeHost) Invoke(ctx context.Context, req sdk.HostInvokeRequest) (*sdk.HostInvokeResponse, error) {
	h.calls = append(h.calls, req)
	if h.invoke != nil {
		return h.invoke(ctx, req)
	}
	return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]interface{}{}}, nil
}

func (h *invokeFakeHost) InvokeStream(context.Context, sdk.HostStreamRequest) (sdk.HostStream, error) {
	return nil, errors.New("not implemented")
}

func TestHostInvokeBaseCases(t *testing.T) {
	g := &OpenAIGateway{}
	if _, err := g.hostInvoke(context.Background(), "method", nil); err == nil {
		t.Fatal("nil host should fail")
	}

	host := &invokeFakeHost{invoke: func(context.Context, sdk.HostInvokeRequest) (*sdk.HostInvokeResponse, error) {
		return nil, nil
	}}
	g.host = host
	payload, err := g.hostInvoke(context.Background(), "method", nil)
	if err != nil {
		t.Fatalf("nil response should not fail: %v", err)
	}
	if len(payload) != 0 {
		t.Fatalf("nil response payload = %#v", payload)
	}

	host.invoke = func(context.Context, sdk.HostInvokeRequest) (*sdk.HostInvokeResponse, error) {
		return nil, errors.New("transport failed")
	}
	if _, err := g.hostInvoke(context.Background(), "method", nil); err == nil {
		t.Fatal("host transport error should fail")
	}

	host.invoke = func(context.Context, sdk.HostInvokeRequest) (*sdk.HostInvokeResponse, error) {
		return &sdk.HostInvokeResponse{Status: "error", Payload: map[string]interface{}{"message": "bad request"}}, nil
	}
	if _, err := g.hostInvoke(context.Background(), "method", nil); err == nil || err.Error() != "bad request" {
		t.Fatalf("host error with message = %v", err)
	}
	host.invoke = func(context.Context, sdk.HostInvokeRequest) (*sdk.HostInvokeResponse, error) {
		return &sdk.HostInvokeResponse{Status: "error", Payload: map[string]interface{}{}}, nil
	}
	if _, err := g.hostInvoke(context.Background(), "method.name", nil); err == nil || err.Error() != "core 方法 method.name 返回错误" {
		t.Fatalf("host error fallback = %v", err)
	}
}

func TestHostTaskMethodsAndRuntime(t *testing.T) {
	host := &invokeFakeHost{invoke: func(_ context.Context, req sdk.HostInvokeRequest) (*sdk.HostInvokeResponse, error) {
		switch req.Method {
		case hostMethodTasksCreate:
			if req.Payload["plugin_id"] != PluginID || req.Payload["task_type"] != "image.generate" {
				t.Fatalf("create payload = %#v", req.Payload)
			}
			return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]interface{}{
				"task": map[string]interface{}{"id": float64(10), "task_type": "image.generate", "status": "pending"},
			}}, nil
		case hostMethodTasksUpdate:
			if req.Payload["task_id"] == nil {
				t.Fatalf("update payload missing task_id: %#v", req.Payload)
			}
			return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]interface{}{}}, nil
		case hostMethodTasksGet:
			return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]interface{}{
				"data": map[string]interface{}{"id": req.Payload["task_id"], "public_task_id": req.Payload["public_task_id"], "attempts": float64(2), "execution": map[string]any{"upstream": "u1"}},
			}}, nil
		case hostMethodTasksList:
			return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"id": float64(1), "public_task_id": "pub_1"},
					map[string]interface{}{"id": float64(2), "public_task_id": "pub_2"},
				},
			}}, nil
		default:
			t.Fatalf("unexpected host method %s", req.Method)
			return nil, nil
		}
	}}
	g := &OpenAIGateway{host: host, logger: slog.Default(), tasks: NewTaskRegistry()}
	g.tasks.Register(testTaskHandler{taskType: "image.generate"})

	task, err := g.createHostTask(context.Background(), "image.generate", 99, map[string]interface{}{"prompt": "hi"}, map[string]string{"kind": "image"}, 1, 3)
	if err != nil {
		t.Fatalf("createHostTask returned err: %v", err)
	}
	if task.ID != 10 || task.TaskType != "image.generate" {
		t.Fatalf("created task = %#v", task)
	}

	if err := g.updateHostTask(context.Background(), 10, sdk.TaskStatusProcessing, 50, map[string]interface{}{"ok": true}, "", WithExecution(map[string]any{"upstream": "u2"})); err != nil {
		t.Fatalf("updateHostTask returned err: %v", err)
	}
	if _, err := g.getHostTask(context.Background(), 99, 10); err != nil {
		t.Fatalf("getHostTask returned err: %v", err)
	}
	if got, err := g.getHostTaskByPublicTaskID(context.Background(), 99, "public"); err != nil || got.PublicTaskID != "public" {
		t.Fatalf("getHostTaskByPublicTaskID got=%#v err=%v", got, err)
	}
	list, err := g.listHostTasks(context.Background(), 99, "image.generate", "completed", 20, 0)
	if err != nil {
		t.Fatalf("listHostTasks returned err: %v", err)
	}
	if list.Total != 2 || len(list.Tasks) != 2 {
		t.Fatalf("listHostTasks = %#v", list)
	}

	rt := &TaskRuntime{g: g, taskID: 10, logger: slog.Default()}
	if err := rt.SetProgress(context.Background(), 20); err != nil {
		t.Fatalf("SetProgress returned err: %v", err)
	}
	if err := rt.SaveExecution(context.Background(), map[string]any{"step": "upload"}); err != nil {
		t.Fatalf("SaveExecution returned err: %v", err)
	}
	if err := rt.Complete(context.Background(), map[string]any{"done": true}); err != nil {
		t.Fatalf("Complete returned err: %v", err)
	}
	if err := rt.Fail(context.Background(), &TaskError{Type: "rate_limited", Message: "limit"}); err != nil {
		t.Fatalf("Fail returned err: %v", err)
	}

	if got := g.TaskTypes(); len(got) != 1 || got[0] != "image.generate" {
		t.Fatalf("TaskTypes = %#v", got)
	}
	if err := g.ProcessTask(context.Background(), sdk.HostTask{ID: 10, TaskType: "image.generate"}); err != nil {
		t.Fatalf("ProcessTask returned err: %v", err)
	}
	if err := (&OpenAIGateway{tasks: NewTaskRegistry(), logger: slog.Default()}).ProcessTask(context.Background(), sdk.HostTask{TaskType: "missing"}); err == nil {
		t.Fatal("unsupported ProcessTask should fail")
	}
}

func TestHostForwardAndAssetMethods(t *testing.T) {
	host := &invokeFakeHost{invoke: func(_ context.Context, req sdk.HostInvokeRequest) (*sdk.HostInvokeResponse, error) {
		switch req.Method {
		case hostMethodGatewayForward:
			if req.Payload["task_id"] != int64(55) || req.Payload["upstream_task_id"] != "up_1" {
				t.Fatalf("forward payload = %#v", req.Payload)
			}
			return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]interface{}{
				"status_code": float64(201),
				"headers":     map[string]interface{}{"X-Test": []interface{}{"ok"}},
				"body":        base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`)),
				"usage":       map[string]any{"model": "gpt-5.4", "input_tokens": 3},
				"usage_id":    float64(77),
			}}, nil
		case hostMethodAssetsStore:
			return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]interface{}{"public_url": "https://assets/u", "object_key": "obj_1"}}, nil
		case hostMethodAssetsStoreURL:
			return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]interface{}{"public_url": "https://assets/u2", "object_key": "obj_2"}}, nil
		case hostMethodAssetsGetBytes:
			if req.Payload["object_key"] == "bytes" {
				return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]interface{}{"content_type": "image/png", "data": []byte("raw")}}, nil
			}
			if req.Payload["object_key"] == "encoded" {
				return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]interface{}{"content_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("raw2"))}}, nil
			}
			return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]interface{}{"data": 123}}, nil
		default:
			t.Fatalf("unexpected host method %s", req.Method)
			return nil, nil
		}
	}}
	g := &OpenAIGateway{host: host}
	resp, err := g.forwardViaHost(context.Background(), 1, 2, 3, "gpt-5.4", http.MethodPost, "/v1/responses", http.Header{"A": []string{"b"}}, []byte(`{}`), true, withHostForwardTask(55, " up_1 "))
	if err != nil {
		t.Fatalf("forwardViaHost returned err: %v", err)
	}
	if resp.StatusCode != 201 || resp.Headers.Get("X-Test") != "ok" || string(resp.Body) != `{"ok":true}` || resp.UsageID != 77 {
		t.Fatalf("forward response = %#v", resp)
	}
	if resp.Usage == nil || resp.Usage.Model != "gpt-5.4" || resp.Usage.InputTokens != 3 {
		t.Fatalf("forward usage = %#v", resp.Usage)
	}

	stored, err := g.storeAsset(context.Background(), 1, "image", "image/png", ".png", []byte("data"))
	if err != nil || stored.PublicURL != "https://assets/u" || stored.ObjectKey != "obj_1" {
		t.Fatalf("storeAsset got=%#v err=%v", stored, err)
	}
	stored, err = g.storeAssetFromURL(context.Background(), 1, "image", "https://source/image.png")
	if err != nil || stored.PublicURL != "https://assets/u2" || stored.ObjectKey != "obj_2" {
		t.Fatalf("storeAssetFromURL got=%#v err=%v", stored, err)
	}
	data, contentType, err := g.fetchAssetBytes(context.Background(), "bytes")
	if err != nil || string(data) != "raw" || contentType != "image/png" {
		t.Fatalf("fetchAssetBytes bytes data=%q contentType=%q err=%v", data, contentType, err)
	}
	data, contentType, err = g.fetchAssetBytes(context.Background(), "encoded")
	if err != nil || string(data) != "raw2" || contentType != "image/png" {
		t.Fatalf("fetchAssetBytes encoded data=%q contentType=%q err=%v", data, contentType, err)
	}
	if _, _, err := g.fetchAssetBytes(context.Background(), "bad"); err == nil {
		t.Fatal("invalid asset data type should fail")
	}
}
