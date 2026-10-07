package gateway

import (
	"fmt"
	"net/http"
	"strconv"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"github.com/tidwall/sjson"
)

// applyImageWireModel only serializes Core's decision. No aliases or defaults.
func applyImageWireModel(req *sdk.ForwardRequest) error {
	if req == nil {
		return fmt.Errorf("image request is required")
	}
	if wire := req.DispatchPlan.UpstreamModel(); wire != "" {
		req.Model = wire
	}
	if req.Model == "" {
		return fmt.Errorf("image wire model is required")
	}
	return rewriteImageModelField(req, req.Model)
}

// rewriteImageModelField 就地改写请求体里的 model 字段。
// multipart（/v1/images/edits）需要重建 body，boundary 会变，因此同步更新 Content-Type 与 Content-Length。
func rewriteImageModelField(req *sdk.ForwardRequest, target string) error {
	contentType := req.Headers.Get("Content-Type")
	if isMultipartContentType(contentType) {
		body, nextContentType, err := setMultipartTextField(req.Body, contentType, "model", target)
		if err != nil {
			return fmt.Errorf("改写 multipart 请求的图片模型失败: %w", err)
		}
		req.Body = body
		req.Headers.Set("Content-Type", nextContentType)
		setContentLengthHeader(req.Headers, len(body))
		return nil
	}
	patched, err := sjson.SetBytes(req.Body, "model", target)
	if err != nil {
		return fmt.Errorf("改写请求体的图片模型失败: %w", err)
	}
	req.Body = patched
	setContentLengthHeader(req.Headers, len(patched))
	return nil
}

// setContentLengthHeader 保持已声明的 Content-Length 与新 body 一致（没有该头时不新增）。
func setContentLengthHeader(headers http.Header, length int) {
	if headers == nil || headers.Get("Content-Length") == "" {
		return
	}
	headers.Set("Content-Length", strconv.Itoa(length))
}

// imageModelWriteFailureOutcome 把改写失败（malformed multipart 等）转成客户端可见的 400，
// 避免带着未改写的模型名继续转发，导致"看起来通了但计费/路由不是目标模型"。
func imageModelWriteFailureOutcome(err error) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       jsonError(err.Error()),
		},
		Reason: err.Error(),
	}
}
