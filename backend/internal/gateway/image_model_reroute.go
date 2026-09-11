package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/sjson"

	"github.com/DevilGenius/airgate-openai/backend/internal/model"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// ──────────────────────────────────────────────────────
// 图片模型重路由（客户端模型名 → 上游实际模型名）
//
// 规则表在 internal/model（imageModelReroutes）：gpt-image-2.5 → gpt-image-2.5-sunburst。
// 客户端照旧请求裸名，插件在图片请求入口把上游模型名与计费模型一起改成目标模型。
// 三个入口都调用本文件的 applyImageRequestModelReroute / canonicalImageModel：
//   - prepareAPIKeyImageRequest（images.go）：API Key 直通（generations / edits）
//   - forwardImagesViaResponsesToolWithURL（images.go）：OAuth 的 Responses + image_generation tool 翻译
//   - buildImageTaskInput（task_image.go）：异步图片任务的输入元数据
// ──────────────────────────────────────────────────────

// applyImageRequestModelReroute 把图片请求的模型对齐到最终生效模型：
//   - 触发条件：客户端模型命中重路由规则（裸名 gpt-image-2.5）；
//   - 最终模型：Core 显式配置的 wire model 优先，否则用重路由目标；
//   - req.Model 与 body.model 一起改，因为两条链路读取位置不同：API Key 账号把请求体
//     原样转发给上游 Images API；OAuth 账号的计费模型（imageGenerationBillingModel）与
//     prompt 里的 "Use requested image model X" 约束都取自 body.model。
//
// 未命中规则时是 no-op；body 改写后客户端模型已不再是裸名，因此可安全重复调用。
func applyImageRequestModelReroute(ctx context.Context, req *sdk.ForwardRequest) error {
	if req == nil {
		return nil
	}
	clientModel := strings.TrimSpace(req.DispatchPlan.ClientModel)
	if clientModel == "" {
		clientModel = strings.TrimSpace(req.Model)
	}
	rerouteTarget, ok := model.RerouteTarget(clientModel)
	if !ok || strings.EqualFold(rerouteTarget, clientModel) {
		return nil
	}
	target := rerouteTarget
	if current := strings.TrimSpace(req.Model); current != "" && !strings.EqualFold(current, clientModel) {
		// Core 的调度规则已经把模型改成别的名字：以运营配置为准，只把 body 对齐过去。
		target = current
	}
	req.Model = target
	if len(req.Body) > 0 {
		if err := rewriteImageModelField(req, target); err != nil {
			return err
		}
	}
	sdk.LoggerFromContext(ctx).Info("image_model_reroute_applied",
		"client_model", clientModel,
		"upstream_model", target,
	)
	return nil
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

// canonicalImageModel 返回图片模型实际使用的名字（命中重路由规则时返回目标）。
// 用于不直接改写请求体、只需要模型名的场景（异步任务输入元数据）。
func canonicalImageModel(name string) string {
	if target, ok := model.RerouteTarget(name); ok {
		return target
	}
	return strings.TrimSpace(name)
}

// setContentLengthHeader 保持已声明的 Content-Length 与新 body 一致（没有该头时不新增）。
func setContentLengthHeader(headers http.Header, length int) {
	if headers == nil || headers.Get("Content-Length") == "" {
		return
	}
	headers.Set("Content-Length", strconv.Itoa(length))
}

// imageModelRerouteFailureOutcome 把改写失败（malformed multipart 等）转成客户端可见的 400，
// 避免带着未改写的模型名继续转发，导致"看起来通了但计费/路由不是目标模型"。
func imageModelRerouteFailureOutcome(err error) sdk.ForwardOutcome {
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
