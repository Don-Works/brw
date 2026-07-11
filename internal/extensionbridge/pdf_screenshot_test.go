package extensionbridge

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestCropBridgeRasterMapsViewportClipToRenderedPixels(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			source.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	data, err := cropBridgeRaster(encoded.Bytes(), map[string]any{
		"fallbackViewport": map[string]any{"width": 100.0, "height": 100.0},
		"clip":             map[string]any{"x": 25.0, "y": 20.0, "width": 50.0, "height": 40.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	imageResult, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := imageResult.Bounds().Size(); got.X != 100 || got.Y != 40 {
		t.Fatalf("cropped size = %v, want 100x40", got)
	}
}
