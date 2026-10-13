package agent

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestJPEG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	path := filepath.Join(t.TempDir(), "test.jpg")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create temp image: %v", err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode temp image: %v", err)
	}
	return path
}

func decodeDataURL(t *testing.T, dataURL string) image.Image {
	t.Helper()
	idx := strings.Index(dataURL, ",")
	if idx < 0 {
		t.Fatalf("data URL has no comma: %q", dataURL)
	}
	raw, err := base64.StdEncoding.DecodeString(dataURL[idx+1:])
	if err != nil {
		t.Fatalf("decode data URL: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode image: %v", err)
	}
	return img
}

func TestDownscaleToDataURL_CapsLongestEdge(t *testing.T) {
	path := writeTestJPEG(t, 800, 400)

	dataURL, ok := downscaleToDataURL(path, "image/jpeg", 100, 85)
	if !ok {
		t.Fatal("expected downscale to succeed for oversized image")
	}
	if !strings.HasPrefix(dataURL, "data:image/jpeg;base64,") {
		t.Fatalf("unexpected data URL prefix: %q", dataURL[:40])
	}

	img := decodeDataURL(t, dataURL)
	b := img.Bounds()
	if b.Dx() > 100 || b.Dy() > 100 {
		t.Fatalf("downscaled image is %dx%d, want longest edge <= 100", b.Dx(), b.Dy())
	}
	// Aspect ratio 2:1 preserved (within rounding).
	if b.Dx() != 100 || b.Dy() != 50 {
		t.Fatalf("downscaled image is %dx%d, want 100x50", b.Dx(), b.Dy())
	}
}

func TestDownscaleToDataURL_SkipsSmallImages(t *testing.T) {
	path := writeTestJPEG(t, 80, 60)

	if _, ok := downscaleToDataURL(path, "image/jpeg", 100, 85); ok {
		t.Fatal("expected downscale to be skipped for an image already within the cap")
	}
}

func TestDownscaleToDataURL_Disabled(t *testing.T) {
	path := writeTestJPEG(t, 800, 400)

	if _, ok := downscaleToDataURL(path, "image/jpeg", 0, 85); ok {
		t.Fatal("expected downscale to be skipped when maxDim is 0")
	}
}

func TestScalingDimensions(t *testing.T) {
	tests := []struct {
		w, h, max, wantW, wantH int
	}{
		{800, 400, 100, 100, 50},
		{400, 800, 100, 50, 100},
		{100, 100, 100, 100, 100},
		{3, 1000, 10, 1, 10},
	}
	for _, tt := range tests {
		gotW, gotH := scaledDimensions(tt.w, tt.h, tt.max)
		if gotW != tt.wantW || gotH != tt.wantH {
			t.Errorf("scaledDimensions(%d,%d,%d) = %d,%d; want %d,%d",
				tt.w, tt.h, tt.max, gotW, gotH, tt.wantW, tt.wantH)
		}
	}
}
