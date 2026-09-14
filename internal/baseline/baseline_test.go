package baseline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/Don-Works/brw/internal/snapshot"
)

// fixtureDigest is a fabricated 64-character content digest.
const fixtureDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func fixtureEnvironment() Environment {
	return Environment{
		BrowserBuild:     "Chrome/141.0.0.0",
		ViewportWidth:    1280,
		ViewportHeight:   800,
		DevicePixelRatio: 1,
		Locale:           "en-gb",
		OS:               "darwin",
	}
}

func newBaselineStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "baselines"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// solidPNG paints one colour, with an optional differently-coloured rectangle,
// so a test can express "this part of the page changed" exactly.
func solidPNG(t *testing.T, width, height int, base color.RGBA, patch image.Rectangle, patchColor color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pixel := base
			if patch.Dx() > 0 && image.Pt(x, y).In(patch) {
				pixel = patchColor
			}
			img.SetRGBA(x, y, pixel)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

var (
	pageWhite = color.RGBA{R: 255, G: 255, B: 255, A: 255}
	brandBlue = color.RGBA{R: 30, G: 90, B: 200, A: 255}
)

func treeWithButtonNamed(name string) snapshot.AriaTree {
	return snapshot.AriaTree{Nodes: []snapshot.AriaNode{{
		Role: "main",
		Children: []snapshot.AriaNode{
			{Role: "heading", Name: "Invoices"},
			{Role: "button", Name: name},
		},
	}}}
}

func TestEnvironmentFingerprintMovesWithEveryDimension(t *testing.T) {
	base := fixtureEnvironment()
	cases := []struct {
		name   string
		mutate func(Environment) Environment
	}{
		{"browser build", func(e Environment) Environment { e.BrowserBuild = "Chrome/142.0.0.0"; return e }},
		{"viewport width", func(e Environment) Environment { e.ViewportWidth = 1440; return e }},
		{"viewport height", func(e Environment) Environment { e.ViewportHeight = 900; return e }},
		{"device pixel ratio", func(e Environment) Environment { e.DevicePixelRatio = 2; return e }},
		{"locale", func(e Environment) Environment { e.Locale = "de-de"; return e }},
		{"os", func(e Environment) Environment { e.OS = "linux"; return e }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			moved := tc.mutate(base)
			if moved.Fingerprint() == base.Fingerprint() {
				t.Fatalf("%s is not in the fingerprint, so a baseline captured under a different one would compare as the same page", tc.name)
			}
			if len(base.Differences(moved)) != 1 {
				t.Fatalf("Differences = %v, want exactly the one field that moved", base.Differences(moved))
			}
		})
	}
	// Normalization must not create spurious mismatches.
	noisy := base
	noisy.Locale = " EN-GB "
	noisy.BrowserBuild = " Chrome/141.0.0.0 "
	noisy.DevicePixelRatio = 1.0000001
	if noisy.Fingerprint() != base.Fingerprint() {
		t.Fatalf("normalization failed: %v", base.Differences(noisy))
	}
}

func TestKeyValidateRejectsAnythingButAPinnedDigest(t *testing.T) {
	cases := []struct {
		name string
		key  Key
		want string
	}{
		{name: "valid", key: Key{RecipeDigest: fixtureDigest, Environment: fixtureEnvironment()}},
		{name: "short digest", key: Key{RecipeDigest: "abc", Environment: fixtureEnvironment()}, want: "64-character hex"},
		{name: "non hex digest", key: Key{RecipeDigest: strings.Repeat("z", 64), Environment: fixtureEnvironment()}, want: "64-character hex"},
		{name: "negative step", key: Key{RecipeDigest: fixtureDigest, StepIndex: -1, Environment: fixtureEnvironment()}, want: "step_index"},
		{name: "incomplete environment", key: Key{RecipeDigest: fixtureDigest}, want: "incomplete environment fingerprint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.key.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestStoreRefusesARootInsideARepository(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatalf("seed repository marker: %v", err)
	}
	nested := filepath.Join(repo, "testdata", "baselines")
	if _, err := NewStore(nested); !errors.Is(err, ErrRootInsideRepository) {
		t.Fatalf("NewStore inside a checkout = %v, want ErrRootInsideRepository", err)
	}
	if _, err := os.Stat(nested); err == nil {
		t.Fatal("a refused root must not be created")
	}

	// The store it does accept carries an ignore-everything file, so a
	// repository created around it later still picks nothing up.
	store := newBaselineStore(t)
	ignore, err := os.ReadFile(filepath.Join(store.Root(), ".gitignore"))
	if err != nil {
		t.Fatalf("read the store's ignore file: %v", err)
	}
	if strings.TrimSpace(string(ignore)) != "*" {
		t.Fatalf("ignore file = %q, want a bare *", ignore)
	}
}

func TestCompareImagesCountsOnlyWhatMoved(t *testing.T) {
	before := solidPNG(t, 40, 20, pageWhite, image.Rectangle{}, pageWhite)
	after := solidPNG(t, 40, 20, pageWhite, image.Rect(0, 0, 4, 5), brandBlue)

	t.Run("identical captures report nothing", func(t *testing.T) {
		diff, err := CompareImages(before, before, VisualOptions{})
		if err != nil {
			t.Fatalf("CompareImages: %v", err)
		}
		if diff.Changed || diff.DiffPixels != 0 {
			t.Fatalf("diff = %+v, want no change", diff)
		}
		if diff.ComparedPixels != 800 {
			t.Fatalf("compared %d pixels, want the whole 40x20 image", diff.ComparedPixels)
		}
	})

	t.Run("a changed rectangle is counted exactly", func(t *testing.T) {
		diff, err := CompareImages(before, after, VisualOptions{})
		if err != nil {
			t.Fatalf("CompareImages: %v", err)
		}
		if !diff.Changed || diff.DiffPixels != 20 {
			t.Fatalf("diff = %+v, want 20 moved pixels", diff)
		}
	})

	t.Run("a tolerance above the fraction passes it", func(t *testing.T) {
		// 20 of 800 pixels is 0.025.
		diff, err := CompareImages(before, after, VisualOptions{PixelTolerance: 0.03})
		if err != nil {
			t.Fatalf("CompareImages: %v", err)
		}
		if diff.Changed {
			t.Fatalf("diff = %+v, want the change absorbed by the tolerance", diff)
		}
		if diff.DiffPixels != 20 {
			t.Fatalf("the tolerance must not hide the count, got %d", diff.DiffPixels)
		}
	})

	t.Run("a named ignore region excludes its pixels", func(t *testing.T) {
		// The capture is 40 image pixels wide and covers 40 CSS pixels, so a CSS
		// rectangle is an image rectangle here.
		diff, err := CompareImages(before, after, VisualOptions{
			IgnoreRegions: []IgnoreRegion{{Name: "clock", X: 0, Y: 0, Width: 4, Height: 5}},
			ViewportWidth: 40,
		})
		if err != nil {
			t.Fatalf("CompareImages: %v", err)
		}
		if diff.Changed || diff.DiffPixels != 0 {
			t.Fatalf("diff = %+v, want the clock excluded", diff)
		}
		if len(diff.IgnoredRegions) != 1 || diff.IgnoredRegions[0] != "clock" {
			t.Fatalf("ignored_regions = %v, want the region named so a reader knows what was skipped", diff.IgnoredRegions)
		}
		if diff.ComparedPixels != 780 {
			t.Fatalf("compared %d pixels, want 780 after excluding the 20-pixel region", diff.ComparedPixels)
		}
	})

	t.Run("a comparison with regions and no viewport is refused", func(t *testing.T) {
		// Fail closed. A guessed factor is wrong in both directions at once: the
		// region a caller named is still compared, and pixels nobody named stop
		// being compared, with nothing in the result saying either happened.
		if _, err := CompareImages(before, after, VisualOptions{
			IgnoreRegions: []IgnoreRegion{{Name: "clock", X: 0, Y: 0, Width: 4, Height: 5}},
		}); err == nil || !strings.Contains(err.Error(), "ViewportWidth") {
			t.Fatalf("error = %v, want a refusal naming the missing viewport width", err)
		}
		// With nothing to place, the viewport is not needed and not demanded.
		if _, err := CompareImages(before, after, VisualOptions{}); err != nil {
			t.Fatalf("CompareImages with no regions: %v", err)
		}
	})

	t.Run("a region that lands off the capture is not reported as ignored", func(t *testing.T) {
		diff, err := CompareImages(before, after, VisualOptions{
			IgnoreRegions: []IgnoreRegion{
				{Name: "clock", X: 0, Y: 0, Width: 4, Height: 5},
				{Name: "ad slot", X: 400, Y: 0, Width: 40, Height: 10},
			},
			ViewportWidth: 40,
		})
		if err != nil {
			t.Fatalf("CompareImages: %v", err)
		}
		if len(diff.IgnoredRegions) != 1 || diff.IgnoredRegions[0] != "clock" {
			t.Fatalf("ignored_regions = %v, want only the region that covered pixels", diff.IgnoredRegions)
		}
		if len(diff.RegionsOutsideCapture) != 1 || diff.RegionsOutsideCapture[0] != "ad slot" {
			t.Fatalf("regions_outside_capture = %v, want the region that excluded nothing", diff.RegionsOutsideCapture)
		}
		if !strings.Contains(diff.Note, "ad slot") {
			t.Fatalf("note = %q, want the region that excluded nothing named", diff.Note)
		}
	})

	t.Run("a resized capture is not compared pixel by pixel", func(t *testing.T) {
		taller := solidPNG(t, 40, 24, pageWhite, image.Rectangle{}, pageWhite)
		diff, err := CompareImages(before, taller, VisualOptions{})
		if err != nil {
			t.Fatalf("CompareImages: %v", err)
		}
		if !diff.Changed || !diff.DimensionsChanged {
			t.Fatalf("diff = %+v, want a dimensions change", diff)
		}
		if diff.ComparedPixels != 0 {
			t.Fatalf("compared %d pixels on a resized capture; there is nothing meaningful to compare", diff.ComparedPixels)
		}
	})

	t.Run("channel tolerance absorbs a one-unit shift", func(t *testing.T) {
		nudged := solidPNG(t, 40, 20, color.RGBA{R: 254, G: 255, B: 255, A: 255}, image.Rectangle{}, pageWhite)
		strict, err := CompareImages(before, nudged, VisualOptions{})
		if err != nil {
			t.Fatalf("CompareImages: %v", err)
		}
		if !strict.Changed {
			t.Fatal("with no channel tolerance a one-unit shift must count")
		}
		lenient, err := CompareImages(before, nudged, VisualOptions{ChannelTolerance: 1})
		if err != nil {
			t.Fatalf("CompareImages: %v", err)
		}
		if lenient.Changed {
			t.Fatalf("diff = %+v, want a one-unit shift absorbed", lenient)
		}
	})
}

// The gate's central property: a deliberate CSS change fails, and keeps failing,
// until someone explicitly accepts it.
func TestAnIntentionalChangeFailsUntilItIsExplicitlyAccepted(t *testing.T) {
	store := newBaselineStore(t)
	key := Key{RecipeDigest: fixtureDigest, StepIndex: 3, Environment: fixtureEnvironment()}
	original := solidPNG(t, 40, 20, pageWhite, image.Rectangle{}, pageWhite)
	restyled := solidPNG(t, 40, 20, pageWhite, image.Rect(0, 0, 10, 10), brandBlue)
	tree := treeWithButtonNamed("Pay invoice")

	missing, err := Check(store, CheckOptions{Key: key, Screenshot: original, Tree: tree})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if missing.Status != StatusMissing || !missing.Failed {
		t.Fatalf("first check = %+v, want a failing %q", missing, StatusMissing)
	}
	if entries, _ := os.ReadDir(filepath.Join(store.Root(), fixtureDigest)); len(entries) != 0 {
		t.Fatal("a check with no update flag must write nothing at all")
	}

	recorded, err := Check(store, CheckOptions{Key: key, Screenshot: original, Tree: tree, Update: true})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if recorded.Status != StatusRecorded {
		t.Fatalf("recording = %+v, want %q", recorded, StatusRecorded)
	}

	matched, err := Check(store, CheckOptions{Key: key, Screenshot: original, Tree: tree})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if matched.Status != StatusMatch || matched.Failed {
		t.Fatalf("unchanged page = %+v, want a passing %q", matched, StatusMatch)
	}

	for attempt := 0; attempt < 2; attempt++ {
		failed, err := Check(store, CheckOptions{Key: key, Screenshot: restyled, Tree: tree})
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if failed.Status != StatusDiff || !failed.Failed {
			t.Fatalf("restyled page (attempt %d) = %+v, want a failing %q", attempt, failed, StatusDiff)
		}
		if failed.Visual == nil || !failed.Visual.Changed {
			t.Fatalf("the visual half must be the one reporting the change: %+v", failed.Visual)
		}
		if failed.ARIA == nil || failed.ARIA.Changed {
			t.Fatalf("a pure CSS change must not move the ARIA structure: %+v", failed.ARIA)
		}
	}

	updated, err := Check(store, CheckOptions{Key: key, Screenshot: restyled, Tree: tree, Update: true})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if updated.Status != StatusUpdated {
		t.Fatalf("update = %+v, want %q", updated, StatusUpdated)
	}
	after, err := Check(store, CheckOptions{Key: key, Screenshot: restyled, Tree: tree})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if after.Status != StatusMatch || after.Failed {
		t.Fatalf("after the explicit update = %+v, want a passing %q", after, StatusMatch)
	}
}

// A different display is not a regression. Reporting it as one is how a visual
// gate gets switched off by the team it was meant to protect.
func TestADifferentDevicePixelRatioReportsAnEnvironmentMismatch(t *testing.T) {
	store := newBaselineStore(t)
	key := Key{RecipeDigest: fixtureDigest, StepIndex: 0, Environment: fixtureEnvironment()}
	shot := solidPNG(t, 40, 20, pageWhite, image.Rectangle{}, pageWhite)
	tree := treeWithButtonNamed("Pay invoice")
	if _, err := Check(store, CheckOptions{Key: key, Screenshot: shot, Tree: tree, Update: true}); err != nil {
		t.Fatalf("record: %v", err)
	}

	retina := key
	retina.Environment.DevicePixelRatio = 2
	// Deliberately a different-sized capture, which is what a 2x display
	// actually produces: a naive comparison would report every pixel moved.
	retinaShot := solidPNG(t, 80, 40, pageWhite, image.Rectangle{}, pageWhite)
	result, err := Check(store, CheckOptions{Key: retina, Screenshot: retinaShot, Tree: tree})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != StatusEnvironmentMismatch || !result.Failed {
		t.Fatalf("result = %+v, want %q", result, StatusEnvironmentMismatch)
	}
	if result.Visual != nil {
		t.Fatalf("no pixel comparison may be reported for a mismatched environment: %+v", result.Visual)
	}
	if len(result.EnvironmentMismatch) != 1 {
		t.Fatalf("environment_mismatch = %+v, want the one stored environment", result.EnvironmentMismatch)
	}
	differences := strings.Join(result.EnvironmentMismatch[0].Differences, "; ")
	if !strings.Contains(differences, "device_pixel_ratio 1 -> 2") {
		t.Fatalf("differences = %q, want the device pixel ratio named", differences)
	}

	// Recording the second environment leaves both usable side by side.
	if _, err := Check(store, CheckOptions{Key: retina, Screenshot: retinaShot, Tree: tree, Update: true}); err != nil {
		t.Fatalf("record retina: %v", err)
	}
	environments, err := store.EnvironmentsFor(fixtureDigest, 0)
	if err != nil {
		t.Fatalf("EnvironmentsFor: %v", err)
	}
	if len(environments) != 2 {
		t.Fatalf("stored %d environments, want both", len(environments))
	}
	back, err := Check(store, CheckOptions{Key: key, Screenshot: shot, Tree: tree})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if back.Status != StatusMatch {
		t.Fatalf("the original environment = %+v, want it still matching", back)
	}
}

// The reason the ARIA half exists: a control can lose its accessible name
// without a single pixel moving.
func TestAnARIAOnlyRegressionFailsWhileThePixelsMatch(t *testing.T) {
	store := newBaselineStore(t)
	key := Key{RecipeDigest: fixtureDigest, StepIndex: 1, Environment: fixtureEnvironment()}
	shot := solidPNG(t, 40, 20, pageWhite, image.Rectangle{}, pageWhite)
	if _, err := Check(store, CheckOptions{
		Key: key, Screenshot: shot, Tree: treeWithButtonNamed("Pay invoice"), Update: true,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	result, err := Check(store, CheckOptions{
		Key: key, Screenshot: shot, Tree: treeWithButtonNamed(""),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != StatusDiff || !result.Failed {
		t.Fatalf("result = %+v, want a failing %q", result, StatusDiff)
	}
	if result.Visual == nil || result.Visual.Changed {
		t.Fatalf("the pixels are byte-identical, so the visual half must pass: %+v", result.Visual)
	}
	if result.ARIA == nil || !result.ARIA.Changed || result.ARIA.Count != 1 {
		t.Fatalf("aria = %+v, want exactly the one name change", result.ARIA)
	}
	change := result.ARIA.Changes[0]
	if change.Kind != snapshot.AriaChangeNameChanged || change.From != "Pay invoice" || change.To != "" {
		t.Fatalf("change = %+v, want the button's accessible name reported as lost", change)
	}
	if !strings.Contains(change.Path, "button") {
		t.Fatalf("path = %q, want it to locate the button", change.Path)
	}
}

func TestCheckRefusesIncompleteInput(t *testing.T) {
	store := newBaselineStore(t)
	key := Key{RecipeDigest: fixtureDigest, Environment: fixtureEnvironment()}
	if _, err := Check(nil, CheckOptions{Key: key, Screenshot: []byte{1}}); err == nil {
		t.Fatal("a nil store must be an error, not a silently passing gate")
	}
	if _, err := Check(store, CheckOptions{Key: key}); err == nil {
		t.Fatal("a check with no screenshot must be an error")
	}
	if err := store.Delete(key); err == nil {
		t.Fatal("deleting a baseline that was never recorded must be an error")
	}
}

// solidJPEG is what both transports actually hand back: Manager.Screenshot and
// Bridge.Screenshot both ask Page.captureScreenshot for jpeg.
func solidJPEG(t *testing.T, width, height int, base color.RGBA, patch image.Rectangle, patchColor color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pixel := base
			if patch.Dx() > 0 && image.Pt(x, y).In(patch) {
				pixel = patchColor
			}
			img.SetRGBA(x, y, pixel)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// NormalizePNG's own behaviour: a JPEG capture becomes a PNG that still gates.
//
// This calls NormalizePNG directly, so it says nothing about whether anything in
// production does. The call site is pinned separately, on the bytes the tool
// writes to the store, by internal/mcp
// TestBaselineToolStoresLosslessPNGWhateverTheTransportCaptured — the
// comparison path decodes JPEG happily, so dropping the production call breaks
// only the file on disk.
func TestNormalizePNGTurnsABrowserCaptureIntoAPNGThatStillGates(t *testing.T) {
	store := newBaselineStore(t)
	key := Key{RecipeDigest: fixtureDigest, StepIndex: 0, Environment: fixtureEnvironment()}

	shot, err := NormalizePNG(solidJPEG(t, 40, 20, pageWhite, image.Rectangle{}, pageWhite))
	if err != nil {
		t.Fatalf("NormalizePNG: %v", err)
	}
	if _, decodeErr := png.Decode(bytes.NewReader(shot)); decodeErr != nil {
		t.Fatalf("a normalized capture is not a PNG: %v", decodeErr)
	}

	recorded, err := Check(store, CheckOptions{Key: key, Screenshot: shot, Tree: treeWithButtonNamed("Pay invoice"), Update: true})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if recorded.Status != StatusRecorded {
		t.Fatalf("record = %+v, want %q", recorded, StatusRecorded)
	}

	// The same page captured again produces the same bytes, so the gate passes.
	again, err := NormalizePNG(solidJPEG(t, 40, 20, pageWhite, image.Rectangle{}, pageWhite))
	if err != nil {
		t.Fatalf("NormalizePNG: %v", err)
	}
	matched, err := Check(store, CheckOptions{Key: key, Screenshot: again, Tree: treeWithButtonNamed("Pay invoice")})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if matched.Status != StatusMatch || matched.Failed {
		t.Fatalf("unchanged page = %+v, want a passing %q", matched, StatusMatch)
	}

	// A real change still fails, so the normalization did not flatten the gate.
	changed, err := NormalizePNG(solidJPEG(t, 40, 20, pageWhite, image.Rect(0, 0, 20, 10), brandBlue))
	if err != nil {
		t.Fatalf("NormalizePNG: %v", err)
	}
	failed, err := Check(store, CheckOptions{Key: key, Screenshot: changed, Tree: treeWithButtonNamed("Pay invoice"), ChannelTolerance: 8})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if failed.Status != StatusDiff || !failed.Failed {
		t.Fatalf("restyled page = %+v, want a failing %q", failed, StatusDiff)
	}

	// The file on disk is what its name says it is.
	stored, found, err := store.Load(key)
	if err != nil || !found {
		t.Fatalf("load: (%v, %v)", found, err)
	}
	if _, err := png.Decode(bytes.NewReader(stored.Screenshot)); err != nil {
		t.Fatalf("the stored screenshot.png is not a PNG: %v", err)
	}
}

// EnvironmentsFor is the one Store method a caller reaches without a Key, and
// the digest it takes becomes a directory name. Anything that is not the pinned
// 64-hex digest is either a typo or a traversal out of the store root.
func TestStoreRefusesADigestThatIsNotAPinnedDigest(t *testing.T) {
	store := newBaselineStore(t)
	outside := filepath.Join(filepath.Dir(store.Root()), "secret", "step-0", "env")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("seed a directory outside the store: %v", err)
	}

	cases := []struct {
		name   string
		digest string
		step   int
	}{
		{name: "parent traversal", digest: "../secret", step: 0},
		{name: "deep traversal", digest: "../../etc", step: 0},
		{name: "absolute path", digest: "/etc", step: 0},
		{name: "empty", digest: "", step: 0},
		{name: "short hex", digest: "0123456789abcdef", step: 0},
		{name: "non hex", digest: strings.Repeat("z", 64), step: 0},
		{name: "negative step", digest: fixtureDigest, step: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.EnvironmentsFor(tc.digest, tc.step)
			if err == nil {
				t.Fatalf("EnvironmentsFor(%q, %d) = %v, want a refusal", tc.digest, tc.step, got)
			}
			if got != nil {
				t.Fatalf("a refused lookup returned %v", got)
			}
		})
	}

	// The valid shape still works, so the guard is not simply refusing
	// everything.
	if _, err := store.EnvironmentsFor(strings.ToUpper(fixtureDigest), 0); err != nil {
		t.Fatalf("EnvironmentsFor with a valid digest: %v", err)
	}
}

// A symlinked root defeats a lexical ancestor walk: the check has to run over
// where the directory really lands, or --baseline-root /tmp/bl pointed three
// levels inside a working tree is accepted and the tool description's "cannot
// end up committed" is false.
func TestStoreRefusesARootSymlinkedIntoARepository(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege this test does not assume on windows")
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatalf("seed repository marker: %v", err)
	}
	inside := filepath.Join(repo, "nested", "baselines")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatalf("seed the real location: %v", err)
	}

	elsewhere := t.TempDir()
	link := filepath.Join(elsewhere, "baselines")
	if err := os.Symlink(inside, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if _, err := NewStore(link); !errors.Is(err, ErrRootInsideRepository) {
		t.Fatalf("NewStore through a symlink into a checkout = %v, want ErrRootInsideRepository", err)
	}

	// A link whose parent is the repository counts too: the root itself need
	// not exist yet, so the check resolves the deepest ancestor that does.
	parentLink := filepath.Join(elsewhere, "parent")
	if err := os.Symlink(filepath.Join(repo, "nested"), parentLink); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if _, err := NewStore(filepath.Join(parentLink, "not-yet")); !errors.Is(err, ErrRootInsideRepository) {
		t.Fatalf("NewStore under a symlinked parent = %v, want ErrRootInsideRepository", err)
	}
}

// A baseline holds a screenshot of a signed-in page. A root any local account
// can walk is refused rather than silently tightened, exactly as
// internal/sessionstate treats its own root.
func TestStoreRefusesAWorldReachableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "baselines")
	if err := os.MkdirAll(root, 0o777); err != nil {
		t.Fatalf("seed a permissive root: %v", err)
	}
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err := NewStore(root)
	if err == nil || !strings.Contains(err.Error(), "reachable beyond its owner") {
		t.Fatalf("NewStore on a 0777 root = %v, want a refusal naming the mode", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := NewStore(root); err != nil {
		t.Fatalf("NewStore on an owner-only root: %v", err)
	}
}

// An ignore region the size of the capture turns the visual half into a no-op.
// Reporting that as a pass is the worst outcome available: unionRegions keeps a
// region recorded with the baseline for every later check, so nothing would ever
// say the pixel comparison stopped looking.
func TestAnIgnoreRegionCoveringEverythingIsNotAPass(t *testing.T) {
	store := newBaselineStore(t)
	key := Key{RecipeDigest: fixtureDigest, StepIndex: 0, Environment: fixtureEnvironment()}
	// The whole CSS viewport, which is what a caller writes down: the capture
	// covering it is 40x20 image pixels.
	everything := []IgnoreRegion{{Name: "everything", X: 0, Y: 0, Width: 1280, Height: 800}}

	if _, err := Check(store, CheckOptions{
		Key:           key,
		Screenshot:    solidPNG(t, 40, 20, pageWhite, image.Rectangle{}, pageWhite),
		Tree:          treeWithButtonNamed("Pay invoice"),
		IgnoreRegions: everything,
		Update:        true,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	// The region is now recorded with the baseline, so this check never names it.
	result, err := Check(store, CheckOptions{
		Key:        key,
		Screenshot: solidPNG(t, 40, 20, brandBlue, image.Rectangle{}, brandBlue),
		Tree:       treeWithButtonNamed("Pay invoice"),
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result.Status != StatusDiff || !result.Failed {
		t.Fatalf("a wholly ignored capture = %+v, want a failing %q", result, StatusDiff)
	}
	if result.Visual == nil || result.Visual.ComparedPixels != 0 {
		t.Fatalf("visual = %+v, want zero compared pixels", result.Visual)
	}
	for _, note := range []string{result.Note, result.Visual.Note} {
		if !strings.Contains(note, "compared nothing") {
			t.Fatalf("note = %q, want it to say the visual half checked nothing", note)
		}
	}
	if !strings.Contains(result.Note, "everything") {
		t.Fatalf("note = %q, want it to name the ignore region that swallowed the page", result.Note)
	}
}

// A resized capture is not compared at all, so counting the pixels that moved
// produces "0 of 0 compared pixels moved" — true about nothing, and printed
// over the sentence that says what happened.
func TestAResizedCaptureReportsTheResizeRatherThanAPixelCount(t *testing.T) {
	store := newBaselineStore(t)
	key := Key{RecipeDigest: fixtureDigest, StepIndex: 0, Environment: fixtureEnvironment()}
	if _, err := Check(store, CheckOptions{
		Key:        key,
		Screenshot: solidPNG(t, 40, 20, pageWhite, image.Rectangle{}, pageWhite),
		Tree:       treeWithButtonNamed("Pay invoice"),
		Update:     true,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	result, err := Check(store, CheckOptions{
		Key:        key,
		Screenshot: solidPNG(t, 40, 30, pageWhite, image.Rectangle{}, pageWhite),
		Tree:       treeWithButtonNamed("Pay invoice"),
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result.Status != StatusDiff || !result.Failed {
		t.Fatalf("a resized capture = %+v, want a failing %q", result, StatusDiff)
	}
	if strings.Contains(result.Note, "0 of 0 compared pixels") {
		t.Fatalf("note = %q, want the reason the comparison could not run", result.Note)
	}
	if !strings.Contains(result.Note, "40x30") || !strings.Contains(result.Note, "40x20") {
		t.Fatalf("note = %q, want both capture sizes named", result.Note)
	}
	if !strings.Contains(result.Note, "ARIA structure is unchanged") {
		t.Fatalf("note = %q, want the structural half reported too", result.Note)
	}
}

// EnvironmentExpression only ever runs in a browser, so that is the only place
// it can be checked. A user-agent regex that stopped matching would fall back to
// a UA prefix, which still fingerprints — the failure is quiet, which is the
// argument for pinning it.
func TestEnvironmentExpressionMeasuresARealBrowser(t *testing.T) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.WindowSize(1024, 768),
		chromedp.WSURLReadTimeout(45*time.Second),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()
	ctx, cancel := context.WithTimeout(browserCtx, 60*time.Second)
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>Environment Fixture</title><p>page</p>`))
	}))
	defer srv.Close()

	var raw map[string]any
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL),
		chromedp.Evaluate(EnvironmentExpression, &raw),
	); err != nil {
		t.Skipf("headless Chrome unavailable: %v", err)
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var environment Environment
	if err := json.Unmarshal(encoded, &environment); err != nil {
		t.Fatalf("decode the environment: %v", err)
	}
	environment.OS = runtime.GOOS
	if err := environment.Validate(); err != nil {
		t.Fatalf("a real browser produced an incomplete fingerprint (%+v): %v", environment, err)
	}
	// The point of the regex is a browser BUILD, not a user-agent prefix: the
	// fallback still fingerprints, so a silent fall-through would key baselines
	// on a 120-character string nobody notices is wrong.
	build := regexp.MustCompile(`^(?:Chrome|Chromium|Edg|Firefox|Version)/[0-9][0-9.]*$`)
	if !build.MatchString(environment.BrowserBuild) {
		t.Fatalf("browser_build = %q, want a Name/version pair rather than a user-agent prefix", environment.BrowserBuild)
	}
	if environment.ViewportWidth <= 0 || environment.ViewportHeight <= 0 {
		t.Fatalf("viewport = %dx%d, want the real one", environment.ViewportWidth, environment.ViewportHeight)
	}
	if environment.DevicePixelRatio <= 0 {
		t.Fatalf("device_pixel_ratio = %v, want a positive ratio", environment.DevicePixelRatio)
	}
}
