package gateway

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	imageTier1KMaxPixels       = 1536 * 1024
	imageTier2KMaxPixels       = 2048 * 2048
	imageFallback2KMaxLongEdge = 2560
	imageDimensionMultiple     = 16
	webReverseMaxImageEdge     = 3840
)

func imageTierPriceForSize(size string) (tier string, price float64, ok bool) {
	width, height, ok := parseImageSize(size)
	if !ok {
		return "", 0, false
	}
	totalPixels := width * height
	switch {
	case totalPixels <= imageTier1KMaxPixels:
		return "1k", 0.10, true
	case totalPixels <= imageTier2KMaxPixels:
		return "2k", 0.20, true
	default:
		return "4k", 0.40, true
	}
}

func imageFallbackSizeFor4K(size string) (string, bool) {
	normalized := normalizeImageSizeForUpstream(size)
	tier, _, ok := imageTierPriceForSize(normalized)
	if !ok || tier != "4k" {
		return "", false
	}
	return downgradeImageSizeTo2K(normalized)
}

func downgradeImageSizeTo2K(size string) (string, bool) {
	width, height, ok := parseImageSize(size)
	if !ok {
		return "", false
	}
	width, height, ok = scaleImageSizeToConstraints(width, height, imageFallback2KMaxLongEdge, imageTier2KMaxPixels, imageDimensionMultiple)
	if !ok {
		return "", false
	}
	size = fmt.Sprintf("%dx%d", width, height)
	tier, _, ok := imageTierPriceForSize(size)
	if !ok || tier != "2k" {
		return "", false
	}
	return size, true
}

func scaleImageSizeToConstraints(width, height, maxLongEdge, maxPixels, multiple int) (int, int, bool) {
	if width <= 0 || height <= 0 {
		return 0, 0, false
	}
	if multiple <= 0 {
		multiple = 1
	}

	longIsWidth := width >= height
	longEdge, shortEdge := width, height
	if !longIsWidth {
		longEdge, shortEdge = height, width
	}

	targetLong := longEdge
	if maxLongEdge > 0 && targetLong > maxLongEdge {
		targetLong = maxLongEdge
	}
	if maxPixels > 0 {
		pixelLimitedLong := int(math.Sqrt(float64(maxPixels) * float64(longEdge) / float64(shortEdge)))
		if pixelLimitedLong < targetLong {
			targetLong = pixelLimitedLong
		}
	}
	targetLong = floorImageDimensionToMultiple(targetLong, multiple)
	if targetLong < multiple {
		return 0, 0, false
	}

	for candidateLong := targetLong; candidateLong >= multiple; candidateLong -= multiple {
		rawShort := scaleImageDimension(shortEdge, longEdge, candidateLong)
		for _, candidateShort := range imageDimensionMultipleCandidates(rawShort, multiple) {
			widthCandidate, heightCandidate := candidateLong, candidateShort
			if !longIsWidth {
				widthCandidate, heightCandidate = candidateShort, candidateLong
			}
			if imageSizeWithinConstraints(widthCandidate, heightCandidate, width, height, maxLongEdge, maxPixels) {
				return widthCandidate, heightCandidate, true
			}
		}
	}
	return 0, 0, false
}

func imageDimensionMultipleCandidates(value, multiple int) []int {
	candidates := []int{
		roundImageDimensionToMultiple(value, multiple),
		floorImageDimensionToMultiple(value, multiple),
		ceilImageDimensionToMultiple(value, multiple),
	}
	out := make([]int, 0, len(candidates))
	seen := map[int]struct{}{}
	for _, candidate := range candidates {
		if candidate <= 0 {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func imageSizeWithinConstraints(width, height, originalWidth, originalHeight, maxLongEdge, maxPixels int) bool {
	if width <= 0 || height <= 0 || width > originalWidth || height > originalHeight {
		return false
	}
	longEdge, shortEdge := width, height
	if shortEdge > longEdge {
		longEdge, shortEdge = shortEdge, longEdge
	}
	if maxLongEdge > 0 && longEdge > maxLongEdge {
		return false
	}
	if maxPixels > 0 && width*height > maxPixels {
		return false
	}
	return longEdge <= shortEdge*3
}

func roundImageDimensionToMultiple(value, multiple int) int {
	if value <= 0 || multiple <= 0 {
		return value
	}
	rounded := ((value + multiple/2) / multiple) * multiple
	if rounded < multiple {
		return multiple
	}
	return rounded
}

func floorImageDimensionToMultiple(value, multiple int) int {
	if value <= 0 || multiple <= 0 {
		return value
	}
	return (value / multiple) * multiple
}

func ceilImageDimensionToMultiple(value, multiple int) int {
	if value <= 0 || multiple <= 0 {
		return value
	}
	return ((value + multiple - 1) / multiple) * multiple
}

func normalizeImageSizeForUpstream(size string) string {
	width, height, ok := parseImageSize(size)
	if !ok {
		return strings.TrimSpace(size)
	}
	width, height = clampImageSize(width, height, webReverseMaxImageEdge)
	return fmt.Sprintf("%dx%d", width, height)
}

func parseImageSize(size string) (int, int, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) != 2 {
		return 0, 0, false
	}

	width, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || width <= 0 {
		return 0, 0, false
	}
	height, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

func clampImageSize(width, height, maxEdge int) (int, int) {
	if width <= maxEdge && height <= maxEdge {
		return width, height
	}
	if width >= height {
		return maxEdge, scaleImageDimension(height, width, maxEdge)
	}
	return scaleImageDimension(width, height, maxEdge), maxEdge
}

func scaleImageDimension(value, oldEdge, newEdge int) int {
	return int((int64(value)*int64(newEdge) + int64(oldEdge)/2) / int64(oldEdge))
}
