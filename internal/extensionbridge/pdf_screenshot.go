package extensionbridge

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// rasterizeBridgePDF converts the first page of Chrome's print-renderer fallback
// to PNG, then applies the original viewport clip. The print renderer is only
// used when Chrome has no compositor surface (for example a locked macOS user
// session); normal screenshots never start a subprocess.
func rasterizeBridgePDF(ctx context.Context, pdfData []byte, params map[string]any) ([]byte, error) {
	if len(pdfData) == 0 {
		return nil, fmt.Errorf("screenshot PDF fallback returned no data")
	}
	dir, err := os.MkdirTemp("", "brw-screenshot-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	pdfPath := filepath.Join(dir, "page.pdf")
	pngPath := filepath.Join(dir, "page.png")
	if err := os.WriteFile(pdfPath, pdfData, 0o600); err != nil {
		return nil, err
	}

	type converter struct {
		name   string
		output string
		args   []string
	}
	var converters []converter
	if path, err := exec.LookPath("pdftoppm"); err == nil {
		converters = append(converters, converter{
			name: path, output: pngPath,
			args: []string{"-f", "1", "-l", "1", "-singlefile", "-png", "-r", "96", pdfPath, strings.TrimSuffix(pngPath, ".png")},
		})
	}
	if runtime.GOOS == "darwin" {
		if path, err := exec.LookPath("sips"); err == nil {
			converters = append(converters, converter{name: path, output: pngPath, args: []string{"-s", "format", "png", pdfPath, "--out", pngPath}})
		}
	}
	for _, binary := range []string{"magick", "convert"} {
		if path, err := exec.LookPath(binary); err == nil {
			converters = append(converters, converter{name: path, output: pngPath, args: []string{"-density", "96", pdfPath + "[0]", pngPath}})
		}
	}
	if len(converters) == 0 {
		return nil, fmt.Errorf("chrome screenshot needed the print-renderer fallback, but no PDF rasterizer is installed (tried pdftoppm, sips, magick, convert)")
	}

	var failures []string
	for _, candidate := range converters {
		_ = os.Remove(candidate.output)
		output, runErr := exec.CommandContext(ctx, candidate.name, candidate.args...).CombinedOutput()
		if runErr != nil {
			failures = append(failures, filepath.Base(candidate.name)+": "+runErr.Error()+" "+strings.TrimSpace(string(output)))
			continue
		}
		pngData, readErr := os.ReadFile(candidate.output)
		if readErr != nil || len(pngData) == 0 {
			if readErr == nil {
				readErr = fmt.Errorf("empty output")
			}
			failures = append(failures, filepath.Base(candidate.name)+": "+readErr.Error())
			continue
		}
		return cropBridgeRaster(pngData, params)
	}
	return nil, fmt.Errorf("rasterize screenshot PDF fallback: %s", strings.Join(failures, "; "))
}

func cropBridgeRaster(pngData []byte, params map[string]any) ([]byte, error) {
	clip, _ := params["clip"].(map[string]any)
	viewport, _ := params["fallbackViewport"].(map[string]any)
	if clip == nil || viewport == nil {
		return pngData, nil
	}
	vw, vh := mapNumber(viewport, "width"), mapNumber(viewport, "height")
	if vw <= 0 || vh <= 0 {
		return pngData, nil
	}
	source, _, err := image.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("decode rasterized screenshot fallback: %w", err)
	}
	bounds := source.Bounds()
	sx := float64(bounds.Dx()) / vw
	sy := float64(bounds.Dy()) / vh
	x := bounds.Min.X + int(mapNumber(clip, "x")*sx)
	y := bounds.Min.Y + int(mapNumber(clip, "y")*sy)
	w := int(mapNumber(clip, "width") * sx)
	h := int(mapNumber(clip, "height") * sy)
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("screenshot fallback clip has invalid size %dx%d", w, h)
	}
	crop := image.Rect(x, y, x+w, y+h).Intersect(bounds)
	if crop.Empty() {
		return nil, fmt.Errorf("screenshot fallback clip is outside the rendered page")
	}
	if crop == bounds {
		return pngData, nil
	}
	dest := image.NewRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
	draw.Draw(dest, dest.Bounds(), source, crop.Min, draw.Src)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, dest); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func mapNumber(values map[string]any, key string) float64 {
	switch value := values[key].(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case jsonNumber:
		parsed, _ := value.Float64()
		return parsed
	default:
		return 0
	}
}

// jsonNumber is the small interface implemented by encoding/json.Number. Using
// an interface keeps this helper compatible with both ordinary map values and a
// decoder configured with UseNumber without importing another concrete type.
type jsonNumber interface {
	Float64() (float64, error)
}
