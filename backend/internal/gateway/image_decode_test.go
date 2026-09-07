package gateway

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"testing"
)

func tinyReviewPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDecodeImageRejectsHugeHeaderBeforePixelAllocation(t *testing.T) {
	for _, dimensions := range [][2]int{{8193, 1}, {8192, 8192}, {1 << 30, 1 << 30}} {
		data := tinyReviewPNG(t)
		binary.BigEndian.PutUint32(data[16:20], uint32(dimensions[0]))
		binary.BigEndian.PutUint32(data[20:24], uint32(dimensions[1]))
		binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
		before := imageWorkingBytes.Load()
		img, release, err := decodeImageForWork(data, 0)
		if err == nil || img != nil || release != nil || imageWorkingBytes.Load() != before {
			t.Fatalf("oversized image accepted: %v", dimensions)
		}
	}
	if _, _, err := resizeMaskToImageSize(tinyReviewPNG(t), "image/png", 8192, 8192); err == nil {
		t.Fatal("oversized resize destination accepted")
	}
}

func TestImageWorkBudgetRetainedThroughTransformsAndReleasedOnError(t *testing.T) {
	before := imageWorkingBytes.Load()
	release, err := reserveImageWork(maxImageWorkingBytes / imageWorkingBytesPerPixel)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = decodeImageForWork(tinyReviewPNG(t), 0)
	if !errors.Is(err, errImageProcessingBusy) {
		t.Fatalf("saturated decode = %v", err)
	}
	release()
	release()
	data := tinyReviewPNG(t)
	_, _, err = decodeImageForWork(data[:33], 0) // Valid IHDR, truncated pixels.
	if err == nil || imageWorkingBytes.Load() != before {
		t.Fatal("decode error leaked memory reservation")
	}
	img, release, err := decodeImageForWork(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if imageWorkingBytes.Load() <= before {
		t.Fatal("returned image lost its reservation")
	}
	if _, _, err := encodeJPEGWithinLimit(img, 1024); err != nil {
		t.Fatal(err)
	}
}
