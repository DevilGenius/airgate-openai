package imgen

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestConversationInitAndPrepare(t *testing.T) {
	c := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/backend-api/conversation/init":
			return imgenHTTPResponse(http.StatusOK, "{}", nil), nil
		case "/backend-api/f/conversation/prepare":
			if got := req.Header.Get("Openai-Sentinel-Chat-Requirements-Token"); got != "chat-token" {
				t.Fatalf("chat requirements header = %q", got)
			}
			if got := req.Header.Get("Openai-Sentinel-Proof-Token"); got != "proof-token" {
				t.Fatalf("proof token header = %q", got)
			}
			body, _ := io.ReadAll(req.Body)
			if !strings.Contains(string(body), `"conversation_id":"conv_1"`) {
				t.Fatalf("prepare body missing conversation_id: %s", body)
			}
			if !strings.Contains(string(body), `sediment://file_img`) {
				t.Fatalf("prepare body missing image pointer: %s", body)
			}
			return imgenHTTPResponse(http.StatusOK, `{"conduit_token":"conduit-1"}`, nil), nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	})
	if err := c.conversationInit(); err != nil {
		t.Fatalf("conversationInit returned err: %v", err)
	}
	conduit, err := c.prepareConversation(
		"draw",
		"chat-token",
		"proof-token",
		"conv_1",
		"parent_1",
		[]*UploadedFile{{FileID: "file_img", FileName: "image.png", MimeType: "image/png", Size: 10, Width: 2, Height: 3}},
	)
	if err != nil {
		t.Fatalf("prepareConversation returned err: %v", err)
	}
	if conduit != "conduit-1" {
		t.Fatalf("conduit = %q", conduit)
	}

	failing := newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusInternalServerError, "failed", nil), nil
	})
	if err := failing.conversationInit(); err == nil {
		t.Fatal("conversationInit non-200 should fail")
	}
	if _, err := failing.prepareConversation("draw", "chat", "", "", "", nil); err == nil {
		t.Fatal("prepareConversation non-200 should fail")
	}
}

func TestStreamConversationExtractsConversationAndRefs(t *testing.T) {
	c := newTestClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/backend-api/f/conversation" {
			t.Fatalf("unexpected request %s", req.URL)
		}
		if req.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("Accept = %q", req.Header.Get("Accept"))
		}
		stream := strings.Join([]string{
			`event: message`,
			`data: {"conversation_id":"conv_1","message":{"content":{"parts":["see file-service://file_1 sediment://sed_1 ",{"asset_pointer":"file-service://file_2"}]}}}`,
			`data: not-json`,
			`data: [DONE]`,
			``,
		}, "\n")
		return imgenHTTPResponse(http.StatusOK, stream, nil), nil
	})

	result, err := c.streamConversation("draw", "chat-token", "conduit", "proof", "", "", nil)
	if err != nil {
		t.Fatalf("streamConversation returned err: %v", err)
	}
	if result.ConversationID != "conv_1" {
		t.Fatalf("ConversationID = %q", result.ConversationID)
	}
	wantRefs := []string{"file-service://file_1", "sediment://sed_1", "file-service://file_2"}
	if !reflect.DeepEqual(result.ImageRefs, wantRefs) {
		t.Fatalf("ImageRefs = %#v, want %#v", result.ImageRefs, wantRefs)
	}

	failing := newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusBadGateway, "bad", nil), nil
	})
	if _, err := failing.streamConversation("draw", "chat", "", "", "", "", nil); err == nil {
		t.Fatal("streamConversation non-200 should fail")
	}
}

func TestExtractImageRefs(t *testing.T) {
	c := &Client{}
	seen := map[string]bool{"file-service://already": true}
	var refs []string
	c.extractImageRefs(map[string]any{}, seen, &refs)
	c.extractImageRefs(map[string]any{
		"message": map[string]any{
			"content": map[string]any{
				"parts": []any{
					"text file-service://file_a sediment://sed_a file-service://already ",
					map[string]any{"asset_pointer": "sediment://sed_b"},
					map[string]any{"asset_pointer": "sediment://sed_b"},
					123,
				},
			},
		},
	}, seen, &refs)
	want := []string{"file-service://file_a", "sediment://sed_a", "sediment://sed_b"}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %#v, want %#v", refs, want)
	}
}

func TestStreamStatusAndAsyncStatus(t *testing.T) {
	for name, body := range map[string]string{
		"is_active":     `{"is_active":true}`,
		"status active": `{"status":"active"}`,
		"in progress":   `{"status":"in_progress"}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(func(req *http.Request) (*http.Response, error) {
				return imgenHTTPResponse(http.StatusOK, body, nil), nil
			})
			active, err := c.streamStatus("conv")
			if err != nil {
				t.Fatalf("streamStatus returned err: %v", err)
			}
			if !active {
				t.Fatal("streamStatus should be active")
			}
		})
	}
	c := newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusOK, `{"status":"complete"}`, nil), nil
	})
	active, err := c.streamStatus("conv")
	if err != nil || active {
		t.Fatalf("inactive streamStatus active=%v err=%v", active, err)
	}
	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusInternalServerError, "", nil), nil
	})
	if _, err := c.streamStatus("conv"); err == nil {
		t.Fatal("streamStatus non-200 should fail")
	}
	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusOK, `{bad`, nil), nil
	})
	if _, err := c.streamStatus("conv"); err == nil {
		t.Fatal("streamStatus bad JSON should fail")
	}

	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusOK, `{"status":"completed","result":{"asset_pointer":"file-service://file_1","asset_pointers":["file-service://file_1","sediment://sed_1"]}}`, nil), nil
	})
	async, err := c.asyncStatus("conv")
	if err != nil {
		t.Fatalf("asyncStatus returned err: %v", err)
	}
	if !async.Completed || async.RawStatus != "completed" || !reflect.DeepEqual(async.AssetPointers, []string{"file-service://file_1", "sediment://sed_1"}) {
		t.Fatalf("async status = %#v", async)
	}

	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusOK, `{"tasks":[{"status":"completed","result":{"asset_pointer":"file-service://file_2"}},{"status":"completed","result":{"asset_pointers":["sediment://sed_2"]}}]}`, nil), nil
	})
	async, err = c.asyncStatus("conv")
	if err != nil {
		t.Fatalf("asyncStatus tasks returned err: %v", err)
	}
	if !async.Completed || !reflect.DeepEqual(async.AssetPointers, []string{"file-service://file_2", "sediment://sed_2"}) {
		t.Fatalf("async tasks = %#v", async)
	}

	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusBadGateway, "bad", nil), nil
	})
	if _, err := c.asyncStatus("conv"); err == nil {
		t.Fatal("asyncStatus non-200 should fail")
	}
	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusOK, `{bad`, nil), nil
	})
	if _, err := c.asyncStatus("conv"); err == nil {
		t.Fatal("asyncStatus bad JSON should fail")
	}
}

func TestReadMappingRefsAndModel(t *testing.T) {
	mapping := map[string]any{
		"skip": map[string]any{"message": map[string]any{"author": map[string]any{"role": "assistant"}}},
		"later": map[string]any{
			"message": map[string]any{
				"create_time": float64(2),
				"recipient":   "image_gen",
				"author":      map[string]any{"role": "tool", "name": "image_gen"},
				"metadata":    map[string]any{"async_task_type": "image_gen", "model_slug": "gpt-image-2"},
				"content": map[string]any{
					"content_type": "multimodal_text",
					"parts": []any{
						"file-service://file_b sediment://sed_b file-service://file_b ",
						map[string]any{"asset_pointer": "sediment://sed_c"},
					},
				},
			},
		},
		"earlier": map[string]any{
			"message": map[string]any{
				"create_time": float64(1),
				"author":      map[string]any{"role": "tool"},
				"metadata":    map[string]any{"async_task_type": "image_gen", "model_slug": "gpt-image-1"},
				"content": map[string]any{
					"content_type": "multimodal_text",
					"parts":        []any{"file-service://file_a sediment://sed_a "},
				},
			},
		},
	}
	tools := extractImageToolMsgs(mapping)
	if len(tools) != 2 || tools[0].MessageID != "earlier" || tools[1].MessageID != "later" {
		t.Fatalf("tools not filtered/sorted: %#v", tools)
	}

	body, _ := json.Marshal(map[string]any{"mapping": mapping})
	c := newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusOK, string(body), nil), nil
	})
	refs, model := c.readMappingRefsAndModel("conv")
	if model != "gpt-image-2" {
		t.Fatalf("model slug = %q", model)
	}
	sort.Strings(refs)
	want := []string{"file-service://file_a", "file-service://file_b", "sediment://sed_a", "sediment://sed_b", "sediment://sed_c"}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %#v, want %#v", refs, want)
	}

	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusInternalServerError, "", nil), nil
	})
	if refs, model := c.readMappingRefsAndModel("conv"); refs != nil || model != "" {
		t.Fatalf("non-200 mapping = %#v %q", refs, model)
	}
	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusOK, `{bad`, nil), nil
	})
	if refs, model := c.readMappingRefsAndModel("conv"); refs != nil || model != "" {
		t.Fatalf("bad JSON mapping = %#v %q", refs, model)
	}
}

func TestPollAndGenerateHelpers(t *testing.T) {
	if !hasFileService([]string{"sediment://a", "file-service://b"}) {
		t.Fatal("hasFileService should detect file-service refs")
	}
	if hasFileService([]string{"sediment://a"}) {
		t.Fatal("hasFileService should ignore sediment refs")
	}
	if got := filterFileService([]string{"sediment://a", "file-service://b", "file-service://c"}); !reflect.DeepEqual(got, []string{"file-service://b", "file-service://c"}) {
		t.Fatalf("filterFileService = %#v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Client{}).pollForImages(ctx, "conv", 1); err == nil {
		t.Fatal("pollForImages should honor canceled context")
	}
	if _, err := (&Client{}).GenerateImage(context.Background(), " ", nil); err == nil {
		t.Fatal("blank prompt should fail")
	}
}
