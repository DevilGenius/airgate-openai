package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func newImageRerouteGateway() *OpenAIGateway {
	return &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func imageRerouteRequest(body []byte, contentType string) *sdk.ForwardRequest {
	headers := http.Header{}
	headers.Set("X-Forwarded-Path", "/v1/images/generations")
	headers.Set("X-Forwarded-Method", http.MethodPost)
	headers.Set("X-Airgate-Operation-Images-Generate", "true")
	headers.Set("Content-Type", contentType)
	return &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{"api_key": "sk-test"}},
		Model:   "gpt-image-2.5",
		Body:    body,
		Headers: headers,
	}
}

// 客户端请求裸名 gpt-image-2.5 时，API Key 直通链路必须把上游模型、请求体与计费模型都改成 sunburst。
func TestForwardHTTPReroutesBareImageModelForAPIKeyAccount(t *testing.T) {
	var upstreamModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstreamModels = append(upstreamModels, gjson.GetBytes(raw, "model").String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"created":1,"data":[{"b64_json":"aGk="}]}`)
	}))
	defer srv.Close()

	g := newImageRerouteGateway()
	req := imageRerouteRequest([]byte(`{"model":"gpt-image-2.5","prompt":"a shiba","n":1,"size":"1024x1024"}`), "application/json")
	req.Account.Credentials["base_url"] = srv.URL

	outcome, err := g.forwardHTTP(context.Background(), req)
	if err != nil {
		t.Fatalf("forwardHTTP error = %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome = %+v, want success", outcome)
	}
	if got := strings.Join(upstreamModels, ","); got != "gpt-image-2.5-sunburst" {
		t.Fatalf("upstream model = %q, want gpt-image-2.5-sunburst", got)
	}
	if req.Model != "gpt-image-2.5-sunburst" {
		t.Fatalf("req.Model = %q, want gpt-image-2.5-sunburst", req.Model)
	}
	if got := gjson.GetBytes(req.Body, "model").String(); got != "gpt-image-2.5-sunburst" {
		t.Fatalf("request body model = %q, want gpt-image-2.5-sunburst", got)
	}
	if outcome.Usage == nil || outcome.Usage.Model != "gpt-image-2.5-sunburst" {
		t.Fatalf("usage model = %+v, want gpt-image-2.5-sunburst", outcome.Usage)
	}
}

func TestApplyImageRequestModelRerouteRewritesBodies(t *testing.T) {
	t.Run("json body", func(t *testing.T) {
		req := imageRerouteRequest([]byte(`{"model":"gpt-image-2.5","prompt":"a shiba"}`), "application/json")
		req.Headers.Set("Content-Length", fmt.Sprint(len(req.Body)))

		if err := applyImageRequestModelReroute(context.Background(), req); err != nil {
			t.Fatalf("applyImageRequestModelReroute error = %v", err)
		}
		if req.Model != "gpt-image-2.5-sunburst" {
			t.Fatalf("req.Model = %q", req.Model)
		}
		if got := gjson.GetBytes(req.Body, "model").String(); got != "gpt-image-2.5-sunburst" {
			t.Fatalf("body model = %q", got)
		}
		if got := gjson.GetBytes(req.Body, "prompt").String(); got != "a shiba" {
			t.Fatalf("prompt lost: %s", req.Body)
		}
		if got, want := req.Headers.Get("Content-Length"), fmt.Sprint(len(req.Body)); got != want {
			t.Fatalf("Content-Length = %q, want %q", got, want)
		}
		// 幂等：body 已是目标模型时再次调用不产生变化。
		before := string(req.Body)
		if err := applyImageRequestModelReroute(context.Background(), req); err != nil {
			t.Fatalf("second apply error = %v", err)
		}
		if string(req.Body) != before {
			t.Fatalf("second apply changed body: %s", req.Body)
		}
	})

	t.Run("multipart body", func(t *testing.T) {
		var buf bytes.Buffer
		writer := multipart.NewWriter(&buf)
		if err := writer.WriteField("model", "gpt-image-2.5"); err != nil {
			t.Fatalf("write model field: %v", err)
		}
		if err := writer.WriteField("prompt", "a shiba"); err != nil {
			t.Fatalf("write prompt field: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close writer: %v", err)
		}
		req := imageRerouteRequest(buf.Bytes(), writer.FormDataContentType())

		if err := applyImageRequestModelReroute(context.Background(), req); err != nil {
			t.Fatalf("applyImageRequestModelReroute error = %v", err)
		}
		if req.Model != "gpt-image-2.5-sunburst" {
			t.Fatalf("req.Model = %q", req.Model)
		}
		fields := readMultipartFields(t, req.Body, req.Headers.Get("Content-Type"))
		if fields["model"] != "gpt-image-2.5-sunburst" || fields["prompt"] != "a shiba" {
			t.Fatalf("multipart fields = %#v", fields)
		}
	})
}

func readMultipartFields(t *testing.T, body []byte, contentType string) map[string]string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("解析 multipart content-type 失败: %v", err)
	}
	fields := map[string]string{}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读取 multipart part 失败: %v", err)
		}
		data, readErr := io.ReadAll(part)
		_ = part.Close()
		if readErr != nil {
			t.Fatalf("读取 multipart part 内容失败: %v", readErr)
		}
		fields[part.FormName()] = string(data)
	}
	return fields
}

func TestApplyImageRequestModelRerouteScopes(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  string
	}{
		{
			name:  "bare 2.5",
			model: "gpt-image-2.5",
			want:  "gpt-image-2.5-sunburst",
		},
		{
			name:  "case and spaces",
			model: " GPT-Image-2.5 ",
			want:  "gpt-image-2.5-sunburst",
		},
		{
			name:  "flare stays",
			model: "gpt-image-2.5-flare",
			want:  "gpt-image-2.5-flare",
		},
		{
			name:  "sunburst stays",
			model: "gpt-image-2.5-sunburst",
			want:  "gpt-image-2.5-sunburst",
		},
		{
			name:  "gpt-image-2 stays",
			model: "gpt-image-2",
			want:  "gpt-image-2",
		},
		{
			name:  "unknown image model stays",
			model: "gpt-image-9",
			want:  "gpt-image-9",
		},
		{
			name:  "empty stays",
			model: "",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			req := imageRerouteRequest([]byte(`{"model":"gpt-image-2.5","prompt":"a shiba"}`), "application/json")
			req.Model = test.model
			before := string(req.Body)

			if err := applyImageRequestModelReroute(context.Background(), req); err != nil {
				t.Fatalf("applyImageRequestModelReroute error = %v", err)
			}
			if req.Model != test.want {
				t.Fatalf("req.Model = %q, want %q", req.Model, test.want)
			}
			if test.want != "gpt-image-2.5-sunburst" && string(req.Body) != before {
				t.Fatalf("body must stay untouched, got %s", req.Body)
			}
		})
	}
}

// Core 显式配置的 wire model 优先于插件内的重路由表，且 body 与最终模型保持一致。
func TestForwardHTTPKeepsCoreWireModelPrecedence(t *testing.T) {
	var upstreamModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstreamModels = append(upstreamModels, gjson.GetBytes(raw, "model").String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"created":1,"data":[{"b64_json":"aGk="}]}`)
	}))
	defer srv.Close()

	g := newImageRerouteGateway()
	req := imageRerouteRequest([]byte(`{"model":"gpt-image-2.5","prompt":"a shiba","n":1,"size":"1024x1024"}`), "application/json")
	req.Account.Credentials["base_url"] = srv.URL
	req.DispatchPlan.ClientModel = "gpt-image-2.5"
	req.DispatchPlan.WireModel = "gpt-image-2.5-flare"

	outcome, err := g.forwardHTTP(context.Background(), req)
	if err != nil {
		t.Fatalf("forwardHTTP error = %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome = %+v, want success", outcome)
	}
	if got := strings.Join(upstreamModels, ","); got != "gpt-image-2.5-flare" {
		t.Fatalf("upstream model = %q, want the Core wire model", got)
	}
	if got := gjson.GetBytes(req.Body, "model").String(); got != "gpt-image-2.5-flare" {
		t.Fatalf("body model = %q, want the Core wire model", got)
	}
}

// 异步图片任务：任务元数据里记录的模型也必须是重路由后的名字。
func TestBuildImageTaskInputReroutesBareImageModel(t *testing.T) {
	req := imageRerouteRequest([]byte(`{"model":"gpt-image-2.5","prompt":"a shiba","n":1,"size":"1024x1024"}`), "application/json")

	input, attributes, err := buildImageTaskInput(req, "/v1/images/generations", false)
	if err != nil {
		t.Fatalf("buildImageTaskInput error = %v", err)
	}
	if got := input["model"]; got != "gpt-image-2.5-sunburst" {
		t.Fatalf("task input model = %v, want gpt-image-2.5-sunburst", got)
	}
	if got := attributes["model"]; got != "gpt-image-2.5-sunburst" {
		t.Fatalf("task attributes model = %q, want gpt-image-2.5-sunburst", got)
	}
}

// 单张 1K/2K/4K 分档计价的输入是 usage 元数据（Core 只按 size + count 判档，与模型名无关），
// 重路由后的 gpt-image-2.5 请求同样要写全这些元数据，否则会掉回 token 计费。
func TestReroutedImageModelKeepsPerImageTierMetadata(t *testing.T) {
	var upstreamBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstreamBodies = append(upstreamBodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"created":1,"data":[{"b64_json":"aGk="}]}`)
	}))
	defer srv.Close()

	g := newImageRerouteGateway()
	// 2048x2048 落在 2K 档（$0.20/张）。
	req := imageRerouteRequest([]byte(`{"model":"gpt-image-2.5","prompt":"a shiba","n":1,"size":"2048x2048"}`), "application/json")
	req.Account.Credentials["base_url"] = srv.URL

	outcome, err := g.forwardHTTP(context.Background(), req)
	if err != nil {
		t.Fatalf("forwardHTTP error = %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess || outcome.Usage == nil {
		t.Fatalf("outcome = %+v, want success with usage", outcome)
	}
	if len(upstreamBodies) != 1 {
		t.Fatalf("upstream attempts = %d, want 1", len(upstreamBodies))
	}
	metadata := outcome.Usage.Metadata
	if got := metadata["openai.image.size"]; got != "2048x2048" {
		t.Fatalf("openai.image.size = %q, want 2048x2048", got)
	}
	if got := metadata["openai.image.count"]; got != "1" {
		t.Fatalf("openai.image.count = %q, want 1", got)
	}
	if got := metadata["openai.image.unit_price"]; got != "0.2" {
		t.Fatalf("openai.image.unit_price = %q, want 0.2 (2K 档)", got)
	}
	if outcome.Usage.Model != "gpt-image-2.5-sunburst" {
		t.Fatalf("usage model = %q, want gpt-image-2.5-sunburst", outcome.Usage.Model)
	}
}

// 非图片请求不经过图片模块的预处理。
func TestPrepareAPIKeyImageRequestSkipsNonImagePaths(t *testing.T) {
	req := imageRerouteRequest([]byte(`{"model":"gpt-image-2.5","prompt":"a shiba"}`), "application/json")
	req.Headers.Set("X-Forwarded-Path", "/v1/chat/completions")
	before := string(req.Body)

	opts, err := prepareAPIKeyImageRequest(context.Background(), req, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("prepareAPIKeyImageRequest error = %v", err)
	}
	if opts != (imagesResponseOptions{}) {
		t.Fatalf("opts = %+v, want zero value", opts)
	}
	if req.Model != "gpt-image-2.5" || string(req.Body) != before {
		t.Fatalf("non-image request must stay untouched: model=%q body=%s", req.Model, req.Body)
	}
}
