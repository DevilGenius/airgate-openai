package gateway

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type stringerValue string

func (s stringerValue) String() string { return "stringer:" + string(s) }

func TestHostPayloadConversionHelpers(t *testing.T) {
	payload := map[string]interface{}{"data": 1, "fallback": 2}
	if got := firstPayloadValue(payload, "missing", "data"); got != 1 {
		t.Fatalf("firstPayloadValue = %#v", got)
	}
	if got := firstPayloadValue(payload, ""); !reflect.DeepEqual(got, payload) {
		t.Fatalf("blank key should return full payload")
	}
	if got := firstPayloadValue(nil, "data"); got != nil {
		t.Fatalf("nil payload should return nil")
	}

	headers := http.Header{"X-Test": []string{"a", "b"}, "X-One": []string{"1"}}
	hp := headerPayload(headers)
	headers.Add("X-Test", "mutated")
	if got := hp["X-Test"].([]string); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("headerPayload should clone values, got %#v", got)
	}

	parsedHeaders := headerFromPayload(map[string]interface{}{
		"A": []interface{}{"x", 2},
		"B": []string{"y", "z"},
		"C": stringerValue("v"),
		"D": "",
	})
	if got := parsedHeaders.Values("A"); !reflect.DeepEqual(got, []string{"x", "2"}) {
		t.Fatalf("header A = %#v", got)
	}
	if got := parsedHeaders.Values("B"); !reflect.DeepEqual(got, []string{"y", "z"}) {
		t.Fatalf("header B = %#v", got)
	}
	if got := parsedHeaders.Get("C"); got != "stringer:v" {
		t.Fatalf("header C = %q", got)
	}
	if got := headerFromPayload("bad"); len(got) != 0 {
		t.Fatalf("bad header payload should produce empty header, got %#v", got)
	}

	if got := bytesFromPayload([]byte("raw")); string(got) != "raw" {
		t.Fatalf("bytes payload = %q", got)
	}
	encodedJSON := base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`))
	if got := bytesFromPayload(encodedJSON); string(got) != `{"ok":true}` {
		t.Fatalf("base64 JSON payload = %q", got)
	}
	encodedText := base64.StdEncoding.EncodeToString([]byte(`plain`))
	if got := bytesFromPayload(encodedText); string(got) != encodedText {
		t.Fatalf("non-JSON base64 should stay text, got %q", got)
	}
	if got := bytesFromPayload(map[string]any{"ok": true}); string(got) != `{"ok":true}` {
		t.Fatalf("map payload = %q", got)
	}
	if got := bytesFromPayload(nil); got != nil {
		t.Fatalf("nil payload = %#v", got)
	}

	if !looksLikeJSON([]byte(` [1] `)) || !looksLikeJSON([]byte(` {"ok":true}`)) || looksLikeJSON([]byte("plain")) {
		t.Fatal("looksLikeJSON returned unexpected results")
	}
}

func TestHostTaskFromPayload(t *testing.T) {
	started := time.Date(2026, 6, 20, 1, 2, 3, 4, time.UTC)
	completed := started.Add(time.Minute)
	task, err := hostTaskFromPayload(map[string]interface{}{
		"id":             float64(123),
		"public_task_id": "public-123",
		"plugin_id":      PluginID,
		"type":           "image.generate",
		"status":         "completed",
		"user_id":        json.Number("99"),
		"input":          map[string]any{"prompt": "hi"},
		"output":         struct{ URL string }{URL: "https://example.test/image.png"},
		"execution":      map[string]any{"upstream": "u1"},
		"error":          "none",
		"progress":       float32(100),
		"attempts":       int32(2),
		"max_attempts":   int64(3),
		"created_at":     started.Format(time.RFC3339Nano),
		"updated_at":     fmt.Sprint(started.Unix()),
		"started_at":     started,
		"completed_at":   completed.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("hostTaskFromPayload returned err: %v", err)
	}
	if task.ID != 123 || task.PublicTaskID != "public-123" || task.UserID != 99 {
		t.Fatalf("unexpected task identity: %#v", task)
	}
	if task.TaskType != "image.generate" || task.Status != sdk.TaskStatusCompleted {
		t.Fatalf("unexpected task type/status: %#v", task)
	}
	if task.Input["prompt"] != "hi" || task.Output["URL"] != "https://example.test/image.png" {
		t.Fatalf("maps not decoded: input=%#v output=%#v", task.Input, task.Output)
	}
	if task.CreatedAt.IsZero() || task.UpdatedAt.IsZero() || task.StartedAt == nil || task.CompletedAt == nil {
		t.Fatalf("times not decoded: %#v", task)
	}

	task, err = hostTaskFromPayload(map[string]interface{}{"task_id": "public-fallback"})
	if err != nil {
		t.Fatalf("hostTaskFromPayload fallback returned err: %v", err)
	}
	if task.PublicTaskID != "public-fallback" {
		t.Fatalf("PublicTaskID fallback = %q", task.PublicTaskID)
	}

	if _, err := hostTaskFromPayload("bad"); err == nil {
		t.Fatal("invalid task payload should fail")
	}
}

func TestScalarPayloadHelpers(t *testing.T) {
	if got := stringFromAny(stringerValue("x")); got != "stringer:x" {
		t.Fatalf("stringFromAny stringer = %q", got)
	}
	if got := stringFromAny(nil); got != "" {
		t.Fatalf("stringFromAny nil = %q", got)
	}
	if got := int64FromAny("42"); got != 42 {
		t.Fatalf("int64FromAny string = %d", got)
	}
	if got := int64FromAny(json.Number("43")); got != 43 {
		t.Fatalf("int64FromAny json.Number = %d", got)
	}
	if got := intFromAny(float64(44)); got != 44 {
		t.Fatalf("intFromAny = %d", got)
	}
	if got := int64FromAny(errors.New("bad")); got != 0 {
		t.Fatalf("unknown int64 value = %d", got)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if got := timeFromAny(now); !got.Equal(now) {
		t.Fatalf("timeFromAny time = %v", got)
	}
	if got := timeFromAny(now.Format(time.RFC3339)); !got.Equal(now) {
		t.Fatalf("timeFromAny RFC3339 = %v", got)
	}
	if got := timeFromAny(fmt.Sprint(now.Unix())); got.Unix() != now.Unix() {
		t.Fatalf("timeFromAny unix = %v", got)
	}
	if got := timeFromAny("bad"); !got.IsZero() {
		t.Fatalf("bad time should be zero, got %v", got)
	}
	if got := timePtrFromAny(""); got != nil {
		t.Fatalf("blank time pointer = %#v", got)
	}
	if got := timePtrFromAny(now.Format(time.RFC3339)); got == nil || !got.Equal(now) {
		t.Fatalf("time pointer = %#v", got)
	}
}

func TestUsageAndMapPayloadHelpers(t *testing.T) {
	usage := usageFromPayload(map[string]any{
		"model":         "gpt-5.4",
		"input_tokens":  float64(10),
		"output_tokens": float64(5),
	})
	if usage == nil || usage.Model != "gpt-5.4" || usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Fatalf("usageFromPayload = %#v", usage)
	}
	if usageFromPayload(nil) != nil {
		t.Fatal("nil usage should stay nil")
	}
	if usageFromPayload(make(chan int)) != nil {
		t.Fatal("unmarshalable usage should return nil")
	}

	type mapLike struct {
		A string `json:"a"`
	}
	m, ok := mapFromAny(mapLike{A: "x"})
	if !ok || m["a"] != "x" {
		t.Fatalf("mapFromAny struct = %#v ok=%v", m, ok)
	}
	if _, ok := mapFromAny(make(chan int)); ok {
		t.Fatal("unmarshalable map should fail")
	}
	if got := mapValueFromAny(nil); got != nil {
		t.Fatalf("mapValueFromAny nil = %#v", got)
	}
}
