package gateway

import (
	"net/http"
	"testing"
)

func TestHostForwardPayloadOmitsTaskMetadataForPlainForward(t *testing.T) {
	t.Parallel()

	payload := hostForwardPayload(1, 2, 3, "gpt-5.4", http.MethodPost, "/v1/responses", http.Header{}, []byte(`{}`), false)

	if _, ok := payload["task_id"]; ok {
		t.Fatal("task_id should be omitted for plain forward")
	}
	if _, ok := payload["upstream_task_id"]; ok {
		t.Fatal("upstream_task_id should be omitted for plain forward")
	}
}

func TestHostForwardPayloadIncludesStructuredTaskMetadata(t *testing.T) {
	t.Parallel()

	headers := http.Header{}
	headers.Set(taskExecHeader, "true")
	payload := hostForwardPayload(1, 2, 3, "gpt-image-2", http.MethodPost, "/v1/images/generations", headers, []byte(`{}`), false, withHostForwardTask(123, " upstream-123 "))

	if got := payload["task_id"]; got != int64(123) {
		t.Fatalf("task_id = %#v, want 123", got)
	}
	if got := payload["upstream_task_id"]; got != "upstream-123" {
		t.Fatalf("upstream_task_id = %#v, want upstream-123", got)
	}
	headerMap, ok := payload["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("headers type = %T, want map[string]interface{}", payload["headers"])
	}
	if got := headerMap[taskExecHeader]; got == nil {
		t.Fatalf("%s should remain in headers", taskExecHeader)
	}
}
