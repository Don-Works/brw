package baseline

import (
	"image"
	"math"
	"testing"
)

// capturedPage paints a capture of a page: one colour, with the changed
// rectangle in image pixels.
func capturedPage(t *testing.T, width, height int, patch image.Rectangle) []byte {
	t.Helper()
	return solidPNG(t, width, height, pageWhite, patch, brandBlue)
}

// An ignore region is written in CSS pixels and applied to a capture that is not
// in CSS pixels. Neither transport captures at the device pixel ratio: both clip
// the viewport at scale = min(1, 800/viewport_width) on top of the DPR
// (internal/browser screenshotMaxWidth, internal/extensionbridge
// bridgeScreenshotMaxWidth), so on any viewport wider than 800 CSS px the DPR is
// not the factor and a region placed with it lands somewhere else entirely.
//
// The rows enumerate what the two transports actually produce — under the cap
// and over it, at DPR 1 and 2 — with the image rectangle each CSS rectangle
// covers written out rather than recomputed, so the test cannot agree with the
// production formula by sharing it. The measured rows are what a throwaway
// capture against real headless Chrome returned: a 1400x900 viewport yields
// 800x514 at DPR 1 and 1600x1029 at DPR 2.
func TestIgnoreRegionsArePlacedByTheCaptureScaleNotTheDevicePixelRatio(t *testing.T) {
	cases := []struct {
		name string
		// The capture conditions.
		viewportWidth           int
		devicePixelRatio        float64
		imageWidth, imageHeight int
		wantScale               float64
		// The region a caller writes down, in CSS pixels, and the image
		// rectangle it must cover.
		region  IgnoreRegion
		covered image.Rectangle
		// A change outside that rectangle, which must still fail the gate.
		elsewhere image.Rectangle
	}{
		{
			name:          "under the cap",
			viewportWidth: 400, devicePixelRatio: 1,
			imageWidth: 400, imageHeight: 300, wantScale: 1,
			region:    IgnoreRegion{Name: "clock", X: 40, Y: 30, Width: 80, Height: 60},
			covered:   image.Rect(50, 40, 110, 80),
			elsewhere: image.Rect(130, 100, 150, 120),
		},
		{
			name:          "under the cap on a retina display",
			viewportWidth: 400, devicePixelRatio: 2,
			imageWidth: 800, imageHeight: 600, wantScale: 2,
			region:    IgnoreRegion{Name: "clock", X: 40, Y: 30, Width: 80, Height: 60},
			covered:   image.Rect(100, 80, 220, 160),
			elsewhere: image.Rect(250, 190, 270, 210),
		},
		{
			name:          "capped from 1600",
			viewportWidth: 1600, devicePixelRatio: 1,
			imageWidth: 800, imageHeight: 450, wantScale: 0.5,
			region:    IgnoreRegion{Name: "clock", X: 200, Y: 100, Width: 80, Height: 60},
			covered:   image.Rect(105, 55, 135, 75),
			elsewhere: image.Rect(150, 90, 170, 110),
		},
		{
			name:          "capped from 1600 on a retina display",
			viewportWidth: 1600, devicePixelRatio: 2,
			imageWidth: 1600, imageHeight: 900, wantScale: 1,
			region:    IgnoreRegion{Name: "clock", X: 200, Y: 100, Width: 80, Height: 60},
			covered:   image.Rect(210, 110, 270, 150),
			elsewhere: image.Rect(300, 180, 320, 200),
		},
		{
			name:          "measured: 1400x900 captured at 800x514",
			viewportWidth: 1400, devicePixelRatio: 1,
			imageWidth: 800, imageHeight: 514, wantScale: 800.0 / 1400.0,
			// A region past the image's own width: clamping it away is what
			// silently dropped the exclusion when the DPR was used as the factor.
			region:    IgnoreRegion{Name: "clock", X: 1000, Y: 100, Width: 100, Height: 50},
			covered:   image.Rect(575, 60, 625, 83),
			elsewhere: image.Rect(650, 100, 670, 120),
		},
		{
			name:          "measured: 1400x900 captured at 1600x1029",
			viewportWidth: 1400, devicePixelRatio: 2,
			imageWidth: 1600, imageHeight: 1029, wantScale: 1600.0 / 1400.0,
			region:    IgnoreRegion{Name: "clock", X: 1000, Y: 100, Width: 100, Height: 50},
			covered:   image.Rect(1150, 120, 1250, 165),
			elsewhere: image.Rect(1300, 200, 1320, 220),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := VisualOptions{IgnoreRegions: []IgnoreRegion{tc.region}, ViewportWidth: tc.viewportWidth}
			before := capturedPage(t, tc.imageWidth, tc.imageHeight, image.Rectangle{})

			inside, err := CompareImages(before, capturedPage(t, tc.imageWidth, tc.imageHeight, tc.covered), opts)
			if err != nil {
				t.Fatalf("CompareImages: %v", err)
			}
			if math.Abs(inside.RegionScale-tc.wantScale) > 1e-9 {
				t.Fatalf("region_scale = %v, want %v (image width / CSS viewport width)", inside.RegionScale, tc.wantScale)
			}
			if inside.Changed || inside.DiffPixels != 0 {
				t.Fatalf("diff = %+v, want the named region to cover the change at %v", inside, tc.covered)
			}
			if len(inside.IgnoredRegions) != 1 || inside.IgnoredRegions[0] != "clock" {
				t.Fatalf("ignored_regions = %v, want the clock reported as the exclusion that applied", inside.IgnoredRegions)
			}
			if len(inside.RegionsOutsideCapture) != 0 {
				t.Fatalf("regions_outside_capture = %v, want the region placed inside the capture", inside.RegionsOutsideCapture)
			}

			outside, err := CompareImages(before, capturedPage(t, tc.imageWidth, tc.imageHeight, tc.elsewhere), opts)
			if err != nil {
				t.Fatalf("CompareImages: %v", err)
			}
			if !outside.Changed {
				t.Fatalf("diff = %+v, want a change at %v outside the region to still fail", outside, tc.elsewhere)
			}
		})
	}

	// Half the rows would pass under either rule. These are the ones that tell
	// the two apart, and a table that loses them stops testing anything.
	discriminating := 0
	for _, tc := range cases {
		if float64(tc.imageWidth)/float64(tc.viewportWidth) != tc.devicePixelRatio {
			discriminating++
		}
	}
	if discriminating < 3 {
		t.Fatalf("%d rows have a capture scale that differs from the device pixel ratio; want at least 3, or the table cannot tell the two rules apart", discriminating)
	}
}

// The same property through the whole gate, which is where the viewport that
// places the regions is read from the key rather than passed in by a caller. A
// 1400x900 page is captured at 800x514, so every region a caller writes down is
// 1.75x further right and further down than the pixels it names.
func TestCheckPlacesIgnoreRegionsOnACappedCapture(t *testing.T) {
	store := newBaselineStore(t)
	environment := fixtureEnvironment()
	environment.ViewportWidth, environment.ViewportHeight = 1400, 900
	key := Key{RecipeDigest: fixtureDigest, StepIndex: 7, Environment: environment}
	clock := IgnoreRegion{Name: "clock", X: 1000, Y: 100, Width: 100, Height: 50}
	tree := treeWithButtonNamed("Pay invoice")

	recorded, err := Check(store, CheckOptions{
		Key: key, Screenshot: capturedPage(t, 800, 514, image.Rectangle{}),
		Tree: tree, IgnoreRegions: []IgnoreRegion{clock}, Update: true,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if recorded.Status != StatusRecorded {
		t.Fatalf("record = %+v, want %q", recorded, StatusRecorded)
	}

	// The clock ticked. Its CSS rectangle lands at image x 571..629, y 57..86.
	ticked, err := Check(store, CheckOptions{
		Key: key, Screenshot: capturedPage(t, 800, 514, image.Rect(575, 60, 625, 83)), Tree: tree,
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if ticked.Status != StatusMatch || ticked.Failed {
		t.Fatalf("a change inside the named clock = %+v, want a passing %q", ticked, StatusMatch)
	}
	if len(ticked.IgnoredRegions) != 1 || ticked.IgnoredRegions[0] != "clock" {
		t.Fatalf("ignored_regions = %v, want the clock, which is the region that applied", ticked.IgnoredRegions)
	}
	if len(ticked.RegionsOutsideCapture) != 0 {
		t.Fatalf("regions_outside_capture = %v, want the clock placed inside the capture", ticked.RegionsOutsideCapture)
	}

	// Anything outside it is still a regression.
	moved, err := Check(store, CheckOptions{
		Key: key, Screenshot: capturedPage(t, 800, 514, image.Rect(650, 100, 670, 120)), Tree: tree,
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if moved.Status != StatusDiff || !moved.Failed {
		t.Fatalf("a change outside the clock = %+v, want a failing %q", moved, StatusDiff)
	}
}
