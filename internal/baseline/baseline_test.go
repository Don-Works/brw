package baseline

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		diff, err := CompareImages(before, after, VisualOptions{
			IgnoreRegions:    []IgnoreRegion{{Name: "clock", X: 0, Y: 0, Width: 4, Height: 5}},
			DevicePixelRatio: 1,
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

	t.Run("ignore regions are CSS pixels scaled by the device pixel ratio", func(t *testing.T) {
		// At DPR 2 the same 2x2.5 CSS rectangle covers the 4x5 image rectangle.
		diff, err := CompareImages(before, after, VisualOptions{
			IgnoreRegions:    []IgnoreRegion{{Name: "clock", X: 0, Y: 0, Width: 2, Height: 3}},
			DevicePixelRatio: 2,
		})
		if err != nil {
			t.Fatalf("CompareImages: %v", err)
		}
		if diff.Changed {
			t.Fatalf("diff = %+v, want the scaled region to cover the change", diff)
		}
		unscaled, err := CompareImages(before, after, VisualOptions{
			IgnoreRegions:    []IgnoreRegion{{Name: "clock", X: 0, Y: 0, Width: 2, Height: 3}},
			DevicePixelRatio: 1,
		})
		if err != nil {
			t.Fatalf("CompareImages: %v", err)
		}
		if !unscaled.Changed {
			t.Fatal("at DPR 1 the same CSS rectangle is too small to cover the change, so this must still fail")
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
