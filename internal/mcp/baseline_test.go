package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
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
type pageController struct {
	browser.Controller
	patched bool
	label   string
	dpr     float64
}

func (c *pageController) Screenshot(context.Context) (browser.Screenshot, error) {
	img := image.NewRGBA(image.Rect(0, 0, 20, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 20; x++ {
			pixel := color.RGBA{R: 255, G: 255, B: 255, A: 255}
			if c.patched && x < 5 && y < 5 {
				pixel = color.RGBA{R: 20, G: 80, B: 190, A: 255}
			}
			img.SetRGBA(x, y, pixel)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return browser.Screenshot{}, err
	}
	return browser.Screenshot{MIMEType: "image/png", Data: buf.Bytes(), Base64: base64.StdEncoding.EncodeToString(buf.Bytes())}, nil
}

func (c *pageController) Evaluate(_ context.Context, expression string) (any, error) {
	if expression == snapshot.AriaTreeExpression {
		return map[string]any{"nodes": []any{
			map[string]any{"role": "button", "name": c.label},
		}}, nil
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
	controller := &pageController{label: "Pay invoice"}
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
