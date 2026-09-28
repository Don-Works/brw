package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testScreenshotPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1300, 800))
	rng := rand.New(rand.NewPCG(1, 2))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1300; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(rng.Uint32()), uint8(rng.Uint32()), uint8(rng.Uint32()), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestScreenshotSavePolicyAndAtomicReplacement(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	data := testScreenshotPNG(t)
	capture := func(ScreenshotSaveOptions) (Screenshot, error) { return Screenshot{Data: data}, nil }
	run := func(path string, allow bool) (SavedScreenshot, error) {
		opts := ScreenshotSaveOptions{SavePath: path}
		if err := opts.Normalize(); err != nil {
			return SavedScreenshot{}, err
		}
		return saveScreenshotFile(context.Background(), opts, home, allow, capture)
	}
	path := filepath.Join(home, "docs", "capture.png")
	result, err := run(path, false)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(saved, data) {
		t.Fatalf("saved bytes differ: %v", err)
	}
	hash := sha256.Sum256(data)
	if result.SHA256 != hex.EncodeToString(hash[:]) || result.Width != 1300 || result.Height != 800 || result.Bytes != len(data) {
		t.Fatalf("bad metadata: %+v", result)
	}
	preview, err := base64.StdEncoding.DecodeString(result.Preview.Base64)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(preview))
	if err != nil || format != "jpeg" || max(cfg.Width, cfg.Height) > 512 || len(preview) > 40*1024 {
		t.Fatalf("preview: %s %+v bytes=%d err=%v", format, cfg, len(preview), err)
	}
	if _, err := run(filepath.Join(outside, "denied.png"), false); err == nil {
		t.Fatal("outside home accepted")
	}
	if _, err := run(filepath.Join(home, "..", filepath.Base(outside), "denied.png"), false); err == nil {
		t.Fatal("traversal accepted")
	}
	if err := os.Symlink(outside, filepath.Join(home, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := run(filepath.Join(home, "escape", "denied.png"), false); err == nil {
		t.Fatal("symlink escape accepted")
	}
	if _, err := run(filepath.Join(outside, "allowed.png"), true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := ScreenshotSaveOptions{SavePath: path}
	if err := opts.Normalize(); err != nil {
		t.Fatal(err)
	}
	_, err = saveScreenshotFile(context.Background(), opts, home, false, func(ScreenshotSaveOptions) (Screenshot, error) { return Screenshot{}, errors.New("capture failed") })
	if err == nil {
		t.Fatal("capture failure ignored")
	}
	original, _ := os.ReadFile(path)
	if string(original) != "original" {
		t.Fatal("failed capture destroyed destination")
	}
	if _, err := run(path, false); err != nil {
		t.Fatal(err)
	}
}

func TestScreenshotSaveOptions(t *testing.T) {
	q := 90
	for _, opts := range []ScreenshotSaveOptions{
		{SavePath: "relative.png"},
		{SavePath: "/tmp/a", Format: "gif"},
		{SavePath: "/tmp/a", Quality: &q},
		{SavePath: "/tmp/a", Scale: 4},
		{SavePath: "/tmp/a", Preview: "large"},
		{SavePath: "/tmp/a", SettleMS: -1},
		{SavePath: "/tmp/a", FullPage: true, Ref: "e1"},
		{SavePath: "/tmp/a", Ref: "e1", Region: &ScreenshotRegion{Width: 1, Height: 1}},
		{SavePath: "/tmp/a", Region: &ScreenshotRegion{Width: 1}},
		{SavePath: "/tmp/a", Format: "jpeg", OmitBackground: true},
		{SavePath: "/tmp/a", Hide: []string{""}},
	} {
		if err := opts.Normalize(); err == nil {
			t.Errorf("accepted %+v", opts)
		}
	}
}

func TestScreenshotPreviewMediumAndTransparent(t *testing.T) {
	data := testScreenshotPNG(t)
	preview, err := screenshotPreview(data, "medium")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(preview))
	if err != nil || max(cfg.Width, cfg.Height) > 1024 || len(preview) > 100*1024 {
		t.Fatalf("invalid medium preview: %+v %d %v", cfg, len(preview), err)
	}
	var transparent bytes.Buffer
	if err := png.Encode(&transparent, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	preview, err = screenshotPreview(transparent.Bytes(), "small")
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(preview))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := img.At(0, 0).RGBA()
	if r < 65000 || g < 65000 || b < 65000 {
		t.Fatal("transparent preview was not composited on white")
	}
}

func TestScreenshotPreparationDoesNotExposeHostPath(t *testing.T) {
	prepare, _ := ScreenshotExpressions(ScreenshotSaveOptions{SavePath: "/private-capture-destination/project/secrets.png", Scale: 2})
	if strings.Contains(prepare, "/private-capture-destination") {
		t.Fatal("browser preparation exposed the host destination")
	}
}
