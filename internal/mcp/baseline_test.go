package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

const baselineToolName = "brw_baseline"

// fixtureBaselineDigest is a fabricated 64-character content digest.
const fixtureBaselineDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// pageController stands in for the browser only: it renders a one-colour page
// with an optional patch, and answers the two expressions brw_baseline
// evaluates. Everything under test — the environment fingerprint, the
// screenshot decode, the check/update routing and the comparison — is real.
//
// encoding is a real transport difference, not a knob: internal/browser
// Manager.Screenshot and internal/extensionbridge Bridge.Screenshot both ask
// Page.captureScreenshot for JPEG, so a controller that only ever produced PNG
// would test a capture no deployment hands back.
type pageController struct {
	browser.Controller
	// url is what ListTabs reports the one tab is showing. brw_baseline routes
	// on the page as well as on the digest, so a controller that could not say
	// what it was on would be a browser no deployment has.
	url     string
	patched bool
	// patch repaints an exact rectangle of the capture, in image pixels, so a
	// test can place a change relative to an ignore region instead of relative
	// to the whole left half.
	patch    image.Rectangle
	label    string
	dpr      float64
	encoding string
}

// Screenshot renders the 1280x800 CSS viewport its Evaluate reports into a 20x10
// image. That is not an arbitrary shrink: both transports clip-capture at
// scale = min(1, 800/viewport_width) on top of the device pixel ratio, so a
// capture whose pixels are not CSS pixels is what a real daemon produces on any
// viewport past 800 CSS px.
func (c *pageController) Screenshot(context.Context) (browser.Screenshot, error) {
	img := image.NewRGBA(image.Rect(0, 0, 20, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 20; x++ {
			pixel := color.RGBA{R: 255, G: 255, B: 255, A: 255}
			if (c.patched && x < 10) || image.Pt(x, y).In(c.patch) {
				pixel = color.RGBA{R: 20, G: 80, B: 190, A: 255}
			}
			img.SetRGBA(x, y, pixel)
		}
	}
	var buf bytes.Buffer
	mime := "image/png"
	if c.encoding == "jpeg" {
		mime = "image/jpeg"
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			return browser.Screenshot{}, err
		}
	} else if err := png.Encode(&buf, img); err != nil {
		return browser.Screenshot{}, err
	}
	return browser.Screenshot{MIMEType: mime, Data: buf.Bytes(), Base64: base64.StdEncoding.EncodeToString(buf.Bytes())}, nil
}

func (c *pageController) ListTabs(context.Context) ([]browser.Tab, error) {
	url := c.url
	if url == "" {
		url = "https://fixtures.example.test/report"
	}
	return []browser.Tab{{ID: "tab-1", URL: url, Active: true}}, nil
}

func (c *pageController) Evaluate(_ context.Context, expression string) (any, error) {
	if expression == snapshot.AriaTreeExpression {
		return map[string]any{"nodes": []any{
			map[string]any{"role": "button", "name": c.label},
		}}, nil
	}
	if expression != baseline.EnvironmentExpression {
		return nil, fmt.Errorf("unexpected expression: %s", expression)
	}
	dpr := c.dpr
	if dpr == 0 {
		dpr = 1
	}
	return map[string]any{
		"browser_build":      "Chrome/141.0.0.0",
		"viewport_width":     1280,
		"viewport_height":    800,
		"device_pixel_ratio": dpr,
		"locale":             "en-GB",
	}, nil
}

func baselineServer(t *testing.T, controller *pageController) *Server {
	t.Helper()
	store, err := baseline.NewStore(filepath.Join(t.TempDir(), "baselines"))
	if err != nil {
		t.Fatalf("baseline.NewStore: %v", err)
	}
	s := New(controller)
	s.SetBaselineStore(store)
	return s
}

func callBaselineTool(t *testing.T, s *Server, args string) map[string]any {
	t.Helper()
	result, rpcErr := s.callTool(context.Background(), baselineToolName, json.RawMessage(args))
	if rpcErr != nil {
		t.Fatalf("callTool: %+v", rpcErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var envelope struct {
		IsError           bool           `json:"isError"`
		StructuredContent map[string]any `json:"structuredContent"`
		Content           []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.IsError {
		t.Fatalf("brw_baseline returned a tool error: %s", encoded)
	}
	if envelope.StructuredContent != nil {
		return envelope.StructuredContent
	}
	if len(envelope.Content) == 0 {
		t.Fatalf("empty result: %s", encoded)
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(envelope.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result text: %v", err)
	}
	return out
}

// The whole point of the tool: check never writes, update does, and the
// environment the daemon measures is what the baseline is keyed on.
func TestBaselineToolChecksUpdatesAndNeverWritesOnACheck(t *testing.T) {
	// Both transports capture JPEG; the PNG case is the one a caller gets from
	// a ref-clipped capture. The gate has to hold for whatever arrives, or half
	// the deployments get a decode error instead of a comparison.
	for _, encoding := range []string{"png", "jpeg"} {
		t.Run(encoding, func(t *testing.T) {
			controller := &pageController{label: "Pay invoice", encoding: encoding}
			s := baselineServer(t, controller)
			args := `{"action":"%s","recipe_digest":"` + fixtureBaselineDigest + `","step_index":2}`

			missing := callBaselineTool(t, s, strings.Replace(args, "%s", "check", 1))
			if missing["status"] != baseline.StatusMissing || missing["failed"] != true {
				t.Fatalf("first check = %v, want a failing %q", missing, baseline.StatusMissing)
			}
			if entries, _ := os.ReadDir(filepath.Join(s.baselines.Root(), fixtureBaselineDigest)); len(entries) != 0 {
				t.Fatal("a check wrote to the baseline store; only update may write")
			}

			recorded := callBaselineTool(t, s, strings.Replace(args, "%s", "update", 1))
			if recorded["status"] != baseline.StatusRecorded {
				t.Fatalf("update = %v, want %q", recorded, baseline.StatusRecorded)
			}

			matched := callBaselineTool(t, s, strings.Replace(args, "%s", "check", 1))
			if matched["status"] != baseline.StatusMatch || matched["failed"] != false {
				t.Fatalf("unchanged page = %v, want a passing %q", matched, baseline.StatusMatch)
			}

			controller.patched = true
			failed := callBaselineTool(t, s, strings.Replace(args, "%s", "check", 1))
			if failed["status"] != baseline.StatusDiff || failed["failed"] != true {
				t.Fatalf("restyled page = %v, want a failing %q", failed, baseline.StatusDiff)
			}

			listed := callBaselineTool(t, s, strings.Replace(args, "%s", "list", 1))
			environments, _ := listed["environments"].([]any)
			if len(environments) != 1 {
				t.Fatalf("list = %v, want the one recorded environment", listed)
			}
			first, _ := environments[0].(map[string]any)
			if first["os"] != runtime.GOOS {
				t.Fatalf("recorded os = %v, want the browser host's %q", first["os"], runtime.GOOS)
			}
			if first["locale"] != "en-gb" {
				t.Fatalf("recorded locale = %v, want the normalized page locale", first["locale"])
			}
		})
	}
}

// storedScreenshots returns every screenshot a baseline store holds, keyed by
// its path under the root.
func storedScreenshots(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "screenshot.png" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		relative, _ := filepath.Rel(root, path)
		out[relative] = data
		return nil
	}); err != nil {
		t.Fatalf("walk the baseline store: %v", err)
	}
	return out
}

// Both transports capture the viewport as JPEG. The tool re-encodes that once,
// on the way in, so the store holds the PNG its file name claims — losslessly,
// which is what makes a stored baseline comparable at all rather than a
// re-compressed approximation of the page.
//
// This asserts on the file the tool actually wrote. Calling baseline.NormalizePNG
// from a test says nothing about whether the tool calls it: the comparison path
// decodes JPEG happily, so dropping the call leaves every other test green and
// only the bytes on disk disagree with the .png on the end of their name.
func TestBaselineToolStoresLosslessPNGWhateverTheTransportCaptured(t *testing.T) {
	controller := &pageController{label: "Pay invoice", encoding: "jpeg"}
	s := baselineServer(t, controller)
	args := `{"action":"%s","recipe_digest":"` + fixtureBaselineDigest + `","step_index":4}`

	captured, err := controller.Screenshot(context.Background())
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if _, format, decodeErr := image.Decode(bytes.NewReader(captured.Data)); decodeErr != nil || format != "jpeg" {
		t.Fatalf("the controller captured %q (%v); this test exists because the transports capture JPEG", format, decodeErr)
	}

	recorded := callBaselineTool(t, s, strings.Replace(args, "%s", "update", 1))
	if recorded["status"] != baseline.StatusRecorded {
		t.Fatalf("update = %v, want %q", recorded, baseline.StatusRecorded)
	}

	stored := storedScreenshots(t, s.baselines.Root())
	if len(stored) != 1 {
		t.Fatalf("the store holds %d screenshots, want the one just recorded", len(stored))
	}
	for name, data := range stored {
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			t.Fatalf("%s is not a PNG: %v", name, err)
		}
	}

	// Lossless, not merely PNG-shaped: the decoded pixels are the ones the
	// browser handed over, so the first check after a record cannot fail on the
	// re-encoding.
	matched := callBaselineTool(t, s, strings.Replace(args, "%s", "check", 1))
	if matched["status"] != baseline.StatusMatch || matched["failed"] != false {
		t.Fatalf("the check straight after a record = %v, want a passing %q", matched, baseline.StatusMatch)
	}
}

// A retina run must report the display, not a screen full of moved pixels.
func TestBaselineToolReportsAnEnvironmentMismatchAcrossDevicePixelRatios(t *testing.T) {
	controller := &pageController{label: "Pay invoice"}
	s := baselineServer(t, controller)
	args := `{"action":"%s","recipe_digest":"` + fixtureBaselineDigest + `","step_index":0}`
	callBaselineTool(t, s, strings.Replace(args, "%s", "update", 1))

	controller.dpr = 2
	result := callBaselineTool(t, s, strings.Replace(args, "%s", "check", 1))
	if result["status"] != baseline.StatusEnvironmentMismatch || result["failed"] != true {
		t.Fatalf("result = %v, want %q", result, baseline.StatusEnvironmentMismatch)
	}
	if _, present := result["visual"]; present {
		t.Fatalf("a mismatched environment must not report a pixel diff: %v", result)
	}
	mismatches, _ := result["environment_mismatch"].([]any)
	if len(mismatches) != 1 {
		t.Fatalf("environment_mismatch = %v, want the one stored environment", result)
	}
	first, _ := mismatches[0].(map[string]any)
	differences, _ := first["differences"].([]any)
	if len(differences) != 1 || differences[0] != "device_pixel_ratio 1 -> 2" {
		t.Fatalf("differences = %v, want the device pixel ratio named", differences)
	}
}

// The ARIA half catches what the pixels cannot.
func TestBaselineToolFailsOnAnARIAOnlyRegression(t *testing.T) {
	controller := &pageController{label: "Pay invoice"}
	s := baselineServer(t, controller)
	args := `{"action":"%s","recipe_digest":"` + fixtureBaselineDigest + `","step_index":1}`
	callBaselineTool(t, s, strings.Replace(args, "%s", "update", 1))

	controller.label = ""
	result := callBaselineTool(t, s, strings.Replace(args, "%s", "check", 1))
	if result["status"] != baseline.StatusDiff || result["failed"] != true {
		t.Fatalf("result = %v, want a failing %q", result, baseline.StatusDiff)
	}
	visual, _ := result["visual"].(map[string]any)
	if visual == nil || visual["changed"] != false {
		t.Fatalf("visual = %v, want an unchanged pixel comparison", visual)
	}
	aria, _ := result["aria"].(map[string]any)
	if aria == nil || aria["changed"] != true {
		t.Fatalf("aria = %v, want the structural change reported", aria)
	}
}

// An ignore_regions rectangle is written in CSS pixels by someone reading the
// page, and applied to a capture that is not in CSS pixels: pageController
// renders a 1280 CSS px viewport into a 20px-wide image, the same shape the
// 800px capture cap produces on any real wide viewport. Placing the rectangle by
// the device pixel ratio instead scales it 64x, so the named clock swallows the
// whole capture and the visual half compares nothing — while a caller whose
// region does not start at the left edge gets the opposite, a clock that is
// still compared. Both fail silently.
func TestBaselineToolPlacesIgnoreRegionsInCSSPixels(t *testing.T) {
	controller := &pageController{label: "Pay invoice", encoding: "jpeg"}
	s := baselineServer(t, controller)
	// CSS x 0..512 of a 1280px viewport is image x 0..8 of the 20px capture.
	const args = `{"action":"%s","recipe_digest":"` + fixtureBaselineDigest + `","step_index":5,` +
		`"ignore_regions":[{"name":"clock","x":0,"y":0,"width":512,"height":800}],"channel_tolerance":8}`

	recorded := callBaselineTool(t, s, strings.Replace(args, "%s", "update", 1))
	if recorded["status"] != baseline.StatusRecorded {
		t.Fatalf("update = %v, want %q", recorded, baseline.StatusRecorded)
	}

	controller.patch = image.Rect(0, 0, 8, 10)
	ignored := callBaselineTool(t, s, strings.Replace(args, "%s", "check", 1))
	if ignored["status"] != baseline.StatusMatch || ignored["failed"] != false {
		t.Fatalf("a repaint inside the named clock = %v, want a passing %q", ignored, baseline.StatusMatch)
	}
	names, _ := ignored["ignored_regions"].([]any)
	if len(names) != 1 || names[0] != "clock" {
		t.Fatalf("ignored_regions = %v, want the clock, which is the exclusion that applied", ignored["ignored_regions"])
	}
	if _, present := ignored["regions_outside_capture"]; present {
		t.Fatalf("result = %v, want the clock placed inside the capture", ignored)
	}
	visual, _ := ignored["visual"].(map[string]any)
	if visual == nil || visual["compared_pixels"] == float64(0) {
		t.Fatalf("visual = %v, want the rest of the page still compared", ignored["visual"])
	}

	// The same rectangle must not swallow the whole capture either: a change
	// outside it still fails.
	controller.patch = image.Rect(12, 0, 20, 10)
	moved := callBaselineTool(t, s, strings.Replace(args, "%s", "check", 1))
	if moved["status"] != baseline.StatusDiff || moved["failed"] != true {
		t.Fatalf("a repaint outside the named clock = %v, want a failing %q", moved, baseline.StatusDiff)
	}
}

// The actions are a closed set in three places: the schema's enum, the switch in
// callBaseline, and the refusal for everything else. Enumerating the enum rather
// than listing the verbs here is what stops the next one being added to the
// schema and never reaching the switch — an advertised action that answers
// "unknown action" is a tool lying about itself.
func TestBaselineToolHandlesEveryActionItAdvertises(t *testing.T) {
	schema, _ := baselineTool()["inputSchema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	action, _ := properties["action"].(map[string]any)
	advertised, _ := action["enum"].([]string)
	if len(advertised) == 0 {
		t.Fatal("the action enum is empty, so this test would pass by vacuum")
	}

	for _, name := range advertised {
		t.Run(name, func(t *testing.T) {
			s := baselineServer(t, &pageController{label: "Pay invoice", encoding: "jpeg"})
			// delete needs something to delete; every other action stands alone.
			if name == "delete" {
				callBaselineTool(t, s, `{"action":"update","recipe_digest":"`+fixtureBaselineDigest+`","step_index":0}`)
			}
			args := `{"action":"` + name + `","recipe_digest":"` + fixtureBaselineDigest + `","step_index":0}`
			result, rpcErr := s.callTool(context.Background(), baselineToolName, json.RawMessage(args))
			if rpcErr != nil {
				t.Fatalf("callTool: %+v", rpcErr)
			}
			encoded, _ := json.Marshal(result)
			for _, refusal := range []string{"unknown action", "action is required"} {
				if strings.Contains(string(encoded), refusal) {
					t.Fatalf("%q is advertised in the schema but callBaseline answers %q: %s", name, refusal, encoded)
				}
			}
		})
	}

	// And the set really is closed: an action outside the enum is refused rather
	// than falling through to one of the handled ones.
	s := baselineServer(t, &pageController{label: "Pay invoice"})
	result, rpcErr := s.callTool(context.Background(), baselineToolName,
		json.RawMessage(`{"action":"accept","recipe_digest":"`+fixtureBaselineDigest+`","step_index":0}`))
	if rpcErr != nil {
		t.Fatalf("callTool: %+v", rpcErr)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), "unknown action") {
		t.Fatalf("an action outside the enum = %s, want a refusal", encoded)
	}
}

func TestBaselineToolRefusesBadInput(t *testing.T) {
	s := baselineServer(t, &pageController{label: "Pay invoice"})
	cases := []struct {
		name string
		args string
		want string
	}{
		{"unknown action", `{"action":"accept","recipe_digest":"` + fixtureBaselineDigest + `","step_index":0}`, "unknown action"},
		{"missing action", `{"recipe_digest":"` + fixtureBaselineDigest + `","step_index":0}`, "action is required"},
		{"loose digest", `{"action":"check","recipe_digest":"not-a-digest","step_index":0}`, "64-character hex"},
		// list is the one action that reaches the store without a Key to
		// validate, and the digest it takes becomes a directory name.
		{"traversal digest on list", `{"action":"list","recipe_digest":"../..","step_index":0}`, "64-character hex"},
		{"traversal digest on delete", `{"action":"delete","recipe_digest":"../..","step_index":0}`, "64-character hex"},
		{"negative step on list", `{"action":"list","recipe_digest":"` + fixtureBaselineDigest + `","step_index":-1}`, "must not be negative"},
		{"tolerance out of range", `{"action":"check","recipe_digest":"` + fixtureBaselineDigest + `","step_index":0,"pixel_tolerance":2}`, "between 0 and 1"},
		{"channel tolerance out of range", `{"action":"check","recipe_digest":"` + fixtureBaselineDigest + `","step_index":0,"channel_tolerance":900}`, "between 0 and 255"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, rpcErr := s.callTool(context.Background(), baselineToolName, json.RawMessage(tc.args))
			if rpcErr != nil {
				t.Fatalf("callTool: %+v", rpcErr)
			}
			encoded, _ := json.Marshal(result)
			if !strings.Contains(string(encoded), tc.want) {
				t.Fatalf("error = %s, want one containing %q", encoded, tc.want)
			}
		})
	}
}

// A daemon with no baseline root must say so rather than gate against a store
// that forgets everything on restart.
func TestBaselineToolRefusesWhenNoRootIsConfigured(t *testing.T) {
	s := New(&pageController{})
	result, rpcErr := s.callTool(context.Background(), baselineToolName,
		json.RawMessage(`{"action":"check","recipe_digest":"`+fixtureBaselineDigest+`","step_index":0}`))
	if rpcErr != nil {
		t.Fatalf("callTool: %+v", rpcErr)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), "--baseline-root") {
		t.Fatalf("the refusal must name the flag that enables baselines: %s", encoded)
	}
}
