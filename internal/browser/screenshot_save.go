package browser

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"strings"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// ScreenshotSaveOptions selects a compositor capture and its browser-host destination.
type ScreenshotSaveOptions struct {
	SavePath       string            `json:"save_path"`
	Format         string            `json:"format,omitempty"`
	Quality        *int              `json:"quality,omitempty"`
	Scale          int               `json:"scale,omitempty"`
	FullPage       bool              `json:"full_page,omitempty"`
	Ref            string            `json:"ref,omitempty"`
	Region         *ScreenshotRegion `json:"region,omitempty"`
	OmitBackground bool              `json:"omit_background,omitempty"`
	Hide           []string          `json:"hide,omitempty"`
	SettleMS       int               `json:"settle_ms,omitempty"`
	Preview        string            `json:"preview,omitempty"`
}

// SavedScreenshot contains disk metadata and only the bounded model-facing preview.
type SavedScreenshot struct {
	Path     string      `json:"path"`
	Width    int         `json:"width"`
	Height   int         `json:"height"`
	Bytes    int         `json:"bytes"`
	SHA256   string      `json:"sha256"`
	MIMEType string      `json:"mime_type"`
	Preview  *Screenshot `json:"preview,omitempty"`
}

// ScreenshotSaver saves on the browser host, including through an HTTP proxy.
type ScreenshotSaver interface {
	SaveScreenshot(context.Context, ScreenshotSaveOptions) (SavedScreenshot, error)
}

const screenshotMaxPixels = 32 * 1024 * 1024

// Normalize validates capture options before any filesystem or browser mutation.
func (o *ScreenshotSaveOptions) Normalize() error {
	if !filepath.IsAbs(o.SavePath) {
		return errors.New("save_path must be absolute")
	}
	o.SavePath = filepath.Clean(o.SavePath)
	if o.Format == "" {
		o.Format = "png"
	}
	if o.Format != "png" && o.Format != "jpeg" && o.Format != "webp" {
		return errors.New("format must be png, jpeg or webp")
	}
	if o.Quality != nil && (*o.Quality < 0 || *o.Quality > 100 || o.Format == "png") {
		return errors.New("quality must be 0–100 and is supported only for jpeg/webp")
	}
	if o.Scale == 0 {
		o.Scale = 1
	}
	if o.Scale < 1 || o.Scale > 3 {
		return errors.New("scale must be 1, 2 or 3")
	}
	if o.Preview == "" {
		o.Preview = "small"
	}
	if o.Preview != "none" && o.Preview != "small" && o.Preview != "medium" {
		return errors.New("preview must be none, small or medium")
	}
	if o.SettleMS < 0 || o.SettleMS > 10000 {
		return errors.New("settle_ms must be 0–10000")
	}
	if (o.Ref != "" && o.Region != nil) || (o.FullPage && (o.Ref != "" || o.Region != nil)) {
		return errors.New("full_page, ref and region are mutually exclusive")
	}
	if r := o.Region; r != nil {
		for _, v := range []float64{r.X, r.Y, r.Width, r.Height} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				return errors.New("region requires finite nonnegative coordinates")
			}
		}
		if r.Width <= 0 || r.Height <= 0 {
			return errors.New("region width and height must be positive")
		}
	}
	if o.OmitBackground && o.Format == "jpeg" {
		return errors.New("omit_background requires png or webp")
	}
	if len(o.Hide) > 100 {
		return errors.New("hide accepts at most 100 CSS selectors")
	}
	for _, s := range o.Hide {
		if strings.TrimSpace(s) == "" || len(s) > 4096 {
			return errors.New("hide selectors must be nonempty and at most 4096 bytes")
		}
	}
	return nil
}

// SaveScreenshotFile confines writes to the user's home unless the host opts out.
func SaveScreenshotFile(ctx context.Context, opts ScreenshotSaveOptions, capture func(ScreenshotSaveOptions) (Screenshot, error)) (SavedScreenshot, error) {
	if err := ctx.Err(); err != nil {
		return SavedScreenshot{}, err
	}
	if err := opts.Normalize(); err != nil {
		return SavedScreenshot{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return SavedScreenshot{}, err
	}
	return saveScreenshotFile(ctx, opts, home, os.Getenv("BRW_SCREENSHOT_ALLOW_OUTSIDE_HOME") == "1", capture)
}

func saveScreenshotFile(ctx context.Context, opts ScreenshotSaveOptions, home string, allowOutside bool, capture func(ScreenshotSaveOptions) (Screenshot, error)) (SavedScreenshot, error) {
	rootPath := home
	rel, err := filepath.Rel(home, opts.SavePath)
	if err != nil || !filepath.IsLocal(rel) {
		if !allowOutside {
			return SavedScreenshot{}, errors.New("save_path must be inside the user's home; configure BRW_SCREENSHOT_ALLOW_OUTSIDE_HOME=1 on the browser host to allow other paths")
		}
		rootPath = filepath.VolumeName(opts.SavePath) + string(filepath.Separator)
		rel, err = filepath.Rel(rootPath, opts.SavePath)
		if err != nil {
			return SavedScreenshot{}, err
		}
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return SavedScreenshot{}, err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(rel), 0755); err != nil {
		return SavedScreenshot{}, err
	}
	shot, err := capture(opts)
	if err != nil {
		return SavedScreenshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return SavedScreenshot{}, err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(shot.Data))
	if err != nil {
		return SavedScreenshot{}, fmt.Errorf("decode screenshot: %w", err)
	}
	if format != opts.Format {
		return SavedScreenshot{}, fmt.Errorf("capture returned %s, requested %s", format, opts.Format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > screenshotMaxPixels {
		return SavedScreenshot{}, errors.New("screenshot exceeds 32 megapixels")
	}
	hash := sha256.Sum256(shot.Data)
	result := SavedScreenshot{Path: opts.SavePath, Width: cfg.Width, Height: cfg.Height, Bytes: len(shot.Data), SHA256: hex.EncodeToString(hash[:]), MIMEType: "image/" + format}
	if opts.Preview != "none" {
		preview, err := screenshotPreview(shot.Data, opts.Preview)
		if err != nil {
			return SavedScreenshot{}, err
		}
		result.Preview = &Screenshot{MIMEType: "image/jpeg", Base64: base64.StdEncoding.EncodeToString(preview)}
	}
	tmp := filepath.Join(filepath.Dir(rel), ".brw-screenshot-"+rand.Text())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return SavedScreenshot{}, err
	}
	defer root.Remove(tmp)
	_, writeErr := f.Write(shot.Data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr, ctx.Err()); err != nil {
		return SavedScreenshot{}, err
	}
	if err := root.Rename(tmp, rel); err != nil {
		return SavedScreenshot{}, err
	}
	return result, nil
}

// CheckScreenshotDimensions rejects a capture that Chrome silently rounded or clipped.
func CheckScreenshotDimensions(data []byte, width, height int) error {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if config.Width != width || config.Height != height {
		return fmt.Errorf("capture dimensions %dx%d differ from requested %dx%d", config.Width, config.Height, width, height)
	}
	return nil
}

func screenshotPreview(data []byte, size string) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	edge, limit := 512, 40*1024
	if size == "medium" {
		edge, limit = 1024, 100*1024
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	scale := math.Min(1, float64(edge)/float64(max(w, h)))
	dst := image.NewRGBA(image.Rect(0, 0, max(1, int(math.Round(float64(w)*scale))), max(1, int(math.Round(float64(h)*scale)))))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	for {
		for quality := 75; quality >= 15; quality -= 10 {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
				return nil, err
			}
			if buf.Len() <= limit {
				return buf.Bytes(), nil
			}
		}
		next := image.NewRGBA(image.Rect(0, 0, max(1, dst.Bounds().Dx()*3/4), max(1, dst.Bounds().Dy()*3/4)))
		xdraw.CatmullRom.Scale(next, next.Bounds(), dst, dst.Bounds(), draw.Src, nil)
		dst = next
	}
}
