package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const (
	maxDecodedImageSide   = 8192
	maxDecodedImagePixels = 16 << 20
	maxImageWorkingBytes  = 512 << 20
	// Includes a conservative allowance for decoded 16-bit pixels, RGBA copies,
	// encoder scratch space and resized intermediates while this image is live.
	imageWorkingBytesPerPixel = 32
)

var imageWorkingBytes atomic.Int64
var errImageProcessingBusy = errors.New("图片处理繁忙，请稍后重试")

func imageProcessingBusyOutcome(elapsed time.Duration) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError, // Capacity rejection must not penalize an account.
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Content-Type": {"application/json"}, "Retry-After": {"1"}},
			Body:       openAIErrorJSON("rate_limit_error", "image_processing_busy", errImageProcessingBusy.Error()),
		},
		Reason: errImageProcessingBusy.Error(), Duration: elapsed,
	}
}

func checkedImagePixels(width, height int) (int64, error) {
	if width <= 0 || height <= 0 || width > maxDecodedImageSide || height > maxDecodedImageSide {
		return 0, fmt.Errorf("图片边长必须在 1–%d 像素之间", maxDecodedImageSide)
	}
	pixels := int64(width) * int64(height)
	if pixels > maxDecodedImagePixels {
		return 0, fmt.Errorf("图片总像素不能超过 %d", maxDecodedImagePixels)
	}
	return pixels, nil
}

func reserveImageWork(pixels int64) (func(), error) {
	if pixels <= 0 || pixels > maxImageWorkingBytes/imageWorkingBytesPerPixel {
		return nil, errImageProcessingBusy
	}
	bytes := pixels * imageWorkingBytesPerPixel
	for {
		used := imageWorkingBytes.Load()
		if bytes > maxImageWorkingBytes-used {
			return nil, errImageProcessingBusy
		}
		if imageWorkingBytes.CompareAndSwap(used, used+bytes) {
			var once sync.Once
			return func() { once.Do(func() { imageWorkingBytes.Add(-bytes) }) }, nil
		}
	}
}

// The caller retains release until all transformations/encoders finish using
// the decoded image. Returning a bare Image must not release its reservation.
func decodeImageForWork(data []byte, extraPixels int64) (image.Image, func(), error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	pixels, err := checkedImagePixels(cfg.Width, cfg.Height)
	if err != nil {
		return nil, nil, err
	}
	if extraPixels < 0 || extraPixels > maxDecodedImagePixels {
		return nil, nil, fmt.Errorf("图片目标尺寸超限")
	}
	release, err := reserveImageWork(pixels + extraPixels)
	if err != nil {
		return nil, nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		release()
		return nil, nil, err
	}
	if img.Bounds().Dx() != cfg.Width || img.Bounds().Dy() != cfg.Height {
		release()
		return nil, nil, fmt.Errorf("图片解码尺寸与头部不一致")
	}
	return img, release, nil
}
