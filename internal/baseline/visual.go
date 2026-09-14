package baseline

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"math"
	"sort"
	"strings"

	// Both transports capture the viewport as JPEG (internal/browser
	// Manager.Screenshot and internal/extensionbridge Bridge.Screenshot both ask
	// Page.captureScreenshot for jpeg), so the decoder has to be registered or a
	// baseline check dies on the encoding instead of comparing the page.
	_ "image/jpeg"
)

// IgnoreRegion is a rectangle whose pixels are not compared, named so a report
// says WHICH exclusion swallowed a change rather than only that one did. The
// clock, the avatar and the ad slot are the reason a visual baseline otherwise
// fails on every run.
//
// Coordinates are CSS pixels — the units the page is laid out in and the only
// ones a caller can write down from a screenshot without knowing the device
// pixel ratio. They are scaled by the environment's DPR before comparison.
type IgnoreRegion struct {
	Name   string `json:"name"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func (r IgnoreRegion) valid() bool { return r.Width > 0 && r.Height > 0 }

// VisualOptions drives one pixel comparison.
type VisualOptions struct {
	// PixelTolerance is the fraction of compared pixels allowed to differ
	// before the comparison fails. Zero means any differing pixel fails.
	PixelTolerance float64
	// ChannelTolerance is the per-channel 8-bit delta treated as identical.
	// Anti-aliasing and colour management move a pixel by one or two units on
	// a re-render of unchanged content.
	ChannelTolerance uint8
	IgnoreRegions    []IgnoreRegion
	// DevicePixelRatio scales the CSS-pixel ignore regions into image pixels.
	// Zero is read as 1.
	DevicePixelRatio float64
}

// VisualDiff is the pixel verdict.
type VisualDiff struct {
	Changed        bool    `json:"changed"`
	DiffPixels     int     `json:"diff_pixels"`
	ComparedPixels int     `json:"compared_pixels"`
	IgnoredPixels  int     `json:"ignored_pixels,omitempty"`
	Fraction       float64 `json:"fraction"`
	Tolerance      float64 `json:"tolerance"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	BaselineWidth  int     `json:"baseline_width"`
	BaselineHeight int     `json:"baseline_height"`
	// DimensionsChanged is reported separately because a resized capture cannot
	// be compared pixel by pixel at all; the fraction would be meaningless.
	DimensionsChanged bool     `json:"dimensions_changed,omitempty"`
	IgnoredRegions    []string `json:"ignored_regions,omitempty"`
	Note              string   `json:"note,omitempty"`
}

// maxComparedPixels bounds one comparison. A capture larger than this is
// refused rather than decoded, so a baseline check cannot be turned into a
// memory exhaustion by pointing it at a very tall page.
const maxComparedPixels = 64 << 20

// CompareImages decodes two captures and counts the pixels that moved outside
// the ignore regions. A stored baseline is always PNG (see NormalizePNG); the
// decoder accepts what the browser produces so a comparison fails on the page
// rather than on the encoding.
func CompareImages(baselineCapture, currentCapture []byte, opts VisualOptions) (VisualDiff, error) {
	before, _, err := decodeCapture(baselineCapture, "baseline")
	if err != nil {
		return VisualDiff{}, err
	}
	after, _, err := decodeCapture(currentCapture, "current")
	if err != nil {
		return VisualDiff{}, err
	}
	beforeBounds, afterBounds := before.Bounds(), after.Bounds()
	diff := VisualDiff{
		Tolerance:      opts.PixelTolerance,
		Width:          afterBounds.Dx(),
		Height:         afterBounds.Dy(),
		BaselineWidth:  beforeBounds.Dx(),
		BaselineHeight: beforeBounds.Dy(),
	}
	if beforeBounds.Dx() != afterBounds.Dx() || beforeBounds.Dy() != afterBounds.Dy() {
		diff.Changed = true
		diff.DimensionsChanged = true
		diff.Note = fmt.Sprintf("the capture is %dx%d and the baseline is %dx%d, so no pixel comparison was made",
			diff.Width, diff.Height, diff.BaselineWidth, diff.BaselineHeight)
		return diff, nil
	}

	scale := opts.DevicePixelRatio
	if scale <= 0 {
		scale = 1
	}
	mask, ignored, names := buildIgnoreMask(afterBounds, opts.IgnoreRegions, scale)
	diff.IgnoredPixels = ignored
	diff.IgnoredRegions = names

	tolerance := int(opts.ChannelTolerance)
	compared := 0
	differing := 0
	for y := afterBounds.Min.Y; y < afterBounds.Max.Y; y++ {
		for x := afterBounds.Min.X; x < afterBounds.Max.X; x++ {
			if mask != nil && mask[(y-afterBounds.Min.Y)*afterBounds.Dx()+(x-afterBounds.Min.X)] {
				continue
			}
			compared++
			br, bg, bb, ba := before.At(beforeBounds.Min.X+(x-afterBounds.Min.X), beforeBounds.Min.Y+(y-afterBounds.Min.Y)).RGBA()
			ar, ag, ab, aa := after.At(x, y).RGBA()
			if channelDelta(br, ar) > tolerance || channelDelta(bg, ag) > tolerance ||
				channelDelta(bb, ab) > tolerance || channelDelta(ba, aa) > tolerance {
				differing++
			}
		}
	}
	diff.ComparedPixels = compared
	diff.DiffPixels = differing
	if compared > 0 {
		diff.Fraction = float64(differing) / float64(compared)
	}
	diff.Changed = differing > 0 && diff.Fraction > opts.PixelTolerance
	if differing > 0 && !diff.Changed {
		diff.Note = fmt.Sprintf("%d of %d compared pixels differ, within the %g tolerance", differing, compared, opts.PixelTolerance)
	}
	// Comparing nothing is not a pass. An ignore region the size of the capture
	// leaves compared == 0 and differing == 0, which the line above reads as
	// "unchanged" — and unionRegions keeps a region recorded with the baseline
	// for every later check, so the visual half would stay switched off with
	// nothing in the report saying so.
	if compared == 0 {
		diff.Changed = true
		if ignored > 0 {
			diff.Note = fmt.Sprintf("all %d pixels of the capture are inside an ignore region (%s), so the visual half compared nothing",
				ignored, strings.Join(names, ", "))
		} else {
			diff.Note = "the capture has no pixels, so the visual half compared nothing"
		}
	}
	return diff, nil
}

// channelDelta compares two 16-bit premultiplied channels at 8-bit precision,
// which is the precision a PNG screenshot actually carries.
func channelDelta(a, b uint32) int {
	return int(math.Abs(float64(int(a>>8) - int(b>>8))))
}

// decodeCapture decodes whatever the browser handed back. The pixel cap is
// applied from the header before the pixels are allocated, so an oversized
// capture is refused rather than decoded.
func decodeCapture(data []byte, which string) (image.Image, string, error) {
	if len(data) == 0 {
		return nil, "", fmt.Errorf("%s capture is empty", which)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode %s capture: %w", which, err)
	}
	if int64(config.Width)*int64(config.Height) > maxComparedPixels {
		return nil, "", fmt.Errorf("%s capture is %dx%d, over the pixel cap for a baseline comparison", which, config.Width, config.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode %s capture: %w", which, err)
	}
	return img, format, nil
}

// NormalizePNG re-encodes a browser capture as PNG so a stored baseline is
// always lossless and always the format its file name claims. Both transports
// capture the viewport as JPEG, so without this the visual half of the gate
// never runs anywhere: the store would hold JPEG bytes in a .png file and the
// comparison would refuse them. A capture that is already PNG is returned
// unchanged rather than round-tripped.
func NormalizePNG(data []byte) ([]byte, error) {
	img, format, err := decodeCapture(data, "current")
	if err != nil {
		return nil, err
	}
	if format == "png" {
		return data, nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("re-encode the %s capture as png: %w", format, err)
	}
	return buf.Bytes(), nil
}

// buildIgnoreMask paints the named regions, scaled from CSS pixels into image
// pixels, and reports how many pixels they cover. Overlapping regions are
// counted once.
func buildIgnoreMask(bounds image.Rectangle, regions []IgnoreRegion, scale float64) ([]bool, int, []string) {
	usable := make([]IgnoreRegion, 0, len(regions))
	names := make([]string, 0, len(regions))
	seen := map[string]bool{}
	for _, region := range regions {
		if !region.valid() {
			continue
		}
		usable = append(usable, region)
		name := strings.TrimSpace(region.Name)
		if name == "" {
			name = "unnamed"
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(usable) == 0 {
		return nil, 0, nil
	}
	sort.Strings(names)
	width, height := bounds.Dx(), bounds.Dy()
	mask := make([]bool, width*height)
	covered := 0
	for _, region := range usable {
		x0 := int(math.Floor(float64(region.X) * scale))
		y0 := int(math.Floor(float64(region.Y) * scale))
		x1 := int(math.Ceil(float64(region.X+region.Width) * scale))
		y1 := int(math.Ceil(float64(region.Y+region.Height) * scale))
		x0, y0 = max(x0, 0), max(y0, 0)
		x1, y1 = min(x1, width), min(y1, height)
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				if !mask[y*width+x] {
					mask[y*width+x] = true
					covered++
				}
			}
		}
	}
	return mask, covered, names
}
