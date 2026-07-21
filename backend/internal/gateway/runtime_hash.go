package gateway

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type runtimeHashState struct {
	TextEnabled  bool `json:"text_enabled"`
	ImageEnabled bool `json:"image_enabled"`
}

type runtimeHashCacheStats struct {
	Size     int `json:"size"`
	Capacity int `json:"capacity"`
}

type runtimeHashStats struct {
	Text         runtimeHashCacheStats
	Image        runtimeHashCacheStats
	RequestRetry runtimeHashCacheStats
}

type textHash interface {
	Begin(req *sdk.ForwardRequest, method, path, longContextModel string) textHashBegin
}

type textHashRequest interface {
	EncryptedContent() encryptedContentHashSession
	Finish(outcome sdk.ForwardOutcome, err error) textHashFinish
}

type encryptedContentHashSession interface {
	BeginRewrite()
	ShouldRemove(raw string) bool
	RetrySanitized() bool
	CacheRejected() bool
}

type imageHash interface {
	Begin() imageHashRequest
	CacheTaskRejection(input map[string]any)
}

type imageHashRequest interface {
	Check(req *sdk.ForwardRequest, method, path string) *sdk.ForwardOutcome
	AttachTaskInput(input map[string]any)
	Finish(outcome sdk.ForwardOutcome) bool
}

type textHashBeginEvent uint8

const (
	textHashBeginContinue textHashBeginEvent = iota
	textHashBeginSafetyCacheHit
	textHashBeginContextWindowReroute
)

type textHashBegin struct {
	request             textHashRequest
	outcome             *sdk.ForwardOutcome
	event               textHashBeginEvent
	dispatchClientModel string
	longContextModel    string
}

type textHashFinish struct {
	contextWindowCached          bool
	contextWindowLongModelFailed bool
	encryptedContentSanitized    bool
	encryptedContentCached       bool
	textSafetyCached             bool
	dispatchClientModel          string
	longContextModel             string
}

type runtimeHashBegin struct {
	Context             context.Context
	Outcome             *sdk.ForwardOutcome
	Event               textHashBeginEvent
	DispatchClientModel string
	LongContextModel    string
}

type runtimeHashFinish struct {
	ContextWindowCached          bool
	ContextWindowLongModelFailed bool
	EncryptedContentSanitized    bool
	EncryptedContentCached       bool
	TextSafetyCached             bool
	ImageSafetyCached            bool
	DispatchClientModel          string
	LongContextModel             string
}

type runtimeHashSnapshot struct {
	state runtimeHashState
	text  textHash
	image imageHash
}

// runtimeHash keeps the stateful implementations alive and atomically swaps
// only the strategy snapshot. Each request captures one snapshot, so call
// sites invoke the lifecycle unconditionally and disabled strategies do not
// calculate hashes or touch caches.
type runtimeHash struct {
	once    sync.Once
	mu      sync.Mutex
	current atomic.Pointer[runtimeHashSnapshot]
	text    *enabledTextHash
	image   *enabledImageHash
}

var (
	disabledText                disabledTextHash
	disabledImage               disabledImageHash
	disabledTextRequest         disabledTextHashRequest
	disabledImageRequest        disabledImageHashRequest
	disabledEncryptedContent    disabledEncryptedContentHashSession
	disabledRuntimeHashSnapshot = &runtimeHashSnapshot{
		text:  disabledText,
		image: disabledImage,
	}
	disabledRuntimeHashRequest = &runtimeHashRequest{
		text:  disabledTextRequest,
		image: disabledImageRequest,
	}
)

func (h *runtimeHash) initialize() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.text = &enabledTextHash{}
		h.image = &enabledImageHash{}
		h.current.Store(&runtimeHashSnapshot{
			state: runtimeHashState{TextEnabled: true, ImageEnabled: true},
			text:  h.text,
			image: h.image,
		})
	})
}

func (h *runtimeHash) snapshot() *runtimeHashSnapshot {
	if h == nil {
		return disabledRuntimeHashSnapshot
	}
	h.initialize()
	if current := h.current.Load(); current != nil {
		return current
	}
	return disabledRuntimeHashSnapshot
}

func (h *runtimeHash) State() runtimeHashState {
	return h.snapshot().state
}

func (h *runtimeHash) SetState(state runtimeHashState) runtimeHashState {
	if h == nil {
		return runtimeHashState{}
	}
	h.initialize()
	h.mu.Lock()
	defer h.mu.Unlock()

	selectedText := textHash(disabledText)
	if state.TextEnabled {
		selectedText = h.text
	}
	selectedImage := imageHash(disabledImage)
	if state.ImageEnabled {
		selectedImage = h.image
	}
	h.current.Store(&runtimeHashSnapshot{
		state: state,
		text:  selectedText,
		image: selectedImage,
	})
	return state
}

func (h *runtimeHash) BeginRequest(
	ctx context.Context,
	req *sdk.ForwardRequest,
	method, path, longContextModel string,
) (*runtimeHashRequest, runtimeHashBegin) {
	selected := h.snapshot()
	textBegin := selected.text.Begin(req, method, path, longContextModel)
	if textBegin.request == nil {
		textBegin.request = disabledTextRequest
	}
	imageRequest := selected.image.Begin()
	if imageRequest == nil {
		imageRequest = disabledImageRequest
	}
	request := &runtimeHashRequest{text: textBegin.request, image: imageRequest}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, runtimeHashRequestContextKey{}, request)
	return request, runtimeHashBegin{
		Context:             ctx,
		Outcome:             textBegin.outcome,
		Event:               textBegin.event,
		DispatchClientModel: textBegin.dispatchClientModel,
		LongContextModel:    textBegin.longContextModel,
	}
}

func (h *runtimeHash) CacheImageTaskRejection(input map[string]any) {
	h.snapshot().image.CacheTaskRejection(input)
}

func (h *runtimeHash) Stats(now time.Time) runtimeHashStats {
	if h == nil {
		return runtimeHashStats{}
	}
	h.initialize()
	textSize, textCapacity, requestRetrySize, requestRetryCapacity := h.text.stats(now)
	imageSize, imageCapacity := h.image.stats(now)
	return runtimeHashStats{
		Text: runtimeHashCacheStats{
			Size:     textSize,
			Capacity: textCapacity,
		},
		Image: runtimeHashCacheStats{
			Size:     imageSize,
			Capacity: imageCapacity,
		},
		RequestRetry: runtimeHashCacheStats{
			Size:     requestRetrySize,
			Capacity: requestRetryCapacity,
		},
	}
}

type runtimeHashRequestContextKey struct{}

type runtimeHashRequest struct {
	text  textHashRequest
	image imageHashRequest
}

func runtimeHashRequestFromContext(ctx context.Context) *runtimeHashRequest {
	if ctx == nil {
		return disabledRuntimeHashRequest
	}
	request, _ := ctx.Value(runtimeHashRequestContextKey{}).(*runtimeHashRequest)
	if request == nil {
		return disabledRuntimeHashRequest
	}
	return request
}

func encryptedContentHashSessionFromContext(ctx context.Context) encryptedContentHashSession {
	request := runtimeHashRequestFromContext(ctx)
	if request.text == nil {
		return disabledEncryptedContent
	}
	session := request.text.EncryptedContent()
	if session == nil {
		return disabledEncryptedContent
	}
	return session
}

func (r *runtimeHashRequest) CheckImage(req *sdk.ForwardRequest, method, path string) *sdk.ForwardOutcome {
	if r == nil || r.image == nil {
		return nil
	}
	return r.image.Check(req, method, path)
}

func (r *runtimeHashRequest) AttachTaskInput(input map[string]any) {
	if r == nil || r.image == nil {
		return
	}
	r.image.AttachTaskInput(input)
}

func (r *runtimeHashRequest) Finish(outcome sdk.ForwardOutcome, err error) runtimeHashFinish {
	if r == nil {
		return runtimeHashFinish{}
	}
	textFinish := textHashFinish{}
	if r.text != nil {
		textFinish = r.text.Finish(outcome, err)
	}
	imageCached := false
	if r.image != nil {
		imageCached = r.image.Finish(outcome)
	}
	return runtimeHashFinish{
		ContextWindowCached:          textFinish.contextWindowCached,
		ContextWindowLongModelFailed: textFinish.contextWindowLongModelFailed,
		EncryptedContentSanitized:    textFinish.encryptedContentSanitized,
		EncryptedContentCached:       textFinish.encryptedContentCached,
		TextSafetyCached:             textFinish.textSafetyCached,
		ImageSafetyCached:            imageCached,
		DispatchClientModel:          textFinish.dispatchClientModel,
		LongContextModel:             textFinish.longContextModel,
	}
}

type disabledTextHash struct{}

func (disabledTextHash) Begin(*sdk.ForwardRequest, string, string, string) textHashBegin {
	return textHashBegin{request: disabledTextRequest}
}

type disabledTextHashRequest struct{}

func (disabledTextHashRequest) EncryptedContent() encryptedContentHashSession {
	return disabledEncryptedContent
}

func (disabledTextHashRequest) Finish(sdk.ForwardOutcome, error) textHashFinish {
	return textHashFinish{}
}

type disabledEncryptedContentHashSession struct{}

func (disabledEncryptedContentHashSession) BeginRewrite()            {}
func (disabledEncryptedContentHashSession) ShouldRemove(string) bool { return false }
func (disabledEncryptedContentHashSession) RetrySanitized() bool     { return false }
func (disabledEncryptedContentHashSession) CacheRejected() bool      { return false }

type disabledImageHash struct{}

func (disabledImageHash) Begin() imageHashRequest {
	return disabledImageRequest
}

func (disabledImageHash) CacheTaskRejection(map[string]any) {}

type disabledImageHashRequest struct{}

func (disabledImageHashRequest) Check(*sdk.ForwardRequest, string, string) *sdk.ForwardOutcome {
	return nil
}

func (disabledImageHashRequest) AttachTaskInput(map[string]any) {}

func (disabledImageHashRequest) Finish(sdk.ForwardOutcome) bool {
	return false
}
