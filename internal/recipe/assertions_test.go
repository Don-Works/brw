package recipe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

const assertRecipeJSON = `{
  "schema_version": 1,
  "id": "example.reports.verify-export",
  "version": "1.0.0",
  "name": "Verify report export",
  "description": "Check the exports page and the file it produces.",
  "intents": ["verify report export"],
  "origins": ["https://reports.example.test"],
  "risk": "read_only",
  "inputs": {"digest": {"required": true}},
  "steps": [
    {"id": "url_ok", "action": "assert", "assert": {"kind": "url", "mode": "prefix", "expected": "https://reports.example.test/exports"}},
    {"id": "status_ok", "action": "assert", "assert": {"kind": "http_status", "status": 200}},
    {"id": "rows_ok", "action": "assert", "assert": {"kind": "element_count", "target": {"role": "row", "name_contains": "Invoice"}, "min": 1, "max": 50}},
    {"id": "button_ok", "action": "assert", "assert": {"kind": "element_state", "target": {"role": "button", "name": "Export"}, "state": "enabled"}},
    {"id": "aria_ok", "action": "assert", "assert": {"kind": "attribute", "target": {"role": "button", "name": "Export"}, "attribute": "aria-disabled", "expected": "false"}},
    {"id": "file_ok", "action": "assert", "assert": {"kind": "download", "filename": "export.csv", "sha256": "${input:digest}", "bytes": 2048}}
  ]
}`

// TestRecipeSchemaAcceptsAssertSteps proves the assertion vocabulary is reachable
// from a recipe: the JSON parses with unknown fields disallowed, so the field
// names here are the published ABI, not a Go struct that happens to compile.
func TestRecipeSchemaAcceptsAssertSteps(t *testing.T) {
	value, err := Parse([]byte(assertRecipeJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(value.Steps) != 6 {
		t.Fatalf("parsed %d steps, want 6", len(value.Steps))
	}
	kinds := map[string]bool{}
	for _, step := range value.Steps {
		if step.Action != "assert" || step.Assert == nil {
			t.Fatalf("step %q did not parse as an assertion: %+v", step.ID, step)
		}
		kinds[step.Assert.Kind] = true
	}
	for _, want := range []string{"url", "http_status", "element_count", "element_state", "attribute", "download"} {
		if !kinds[want] {
			t.Fatalf("recipe did not carry an assertion of kind %q", want)
		}
	}
	if err := Validate(value); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestRecipeSchemaRejectsMalformedAssertions(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	tests := []struct {
		name      string
		assertion Assertion
		want      string
	}{
		{name: "unknown kind", assertion: Assertion{Kind: "vibes"}, want: `unsupported assertion kind "vibes"`},
		{name: "url without expected", assertion: Assertion{Kind: "url"}, want: "url assertion requires expected"},
		{name: "url with a bad mode", assertion: Assertion{Kind: "url", Expected: "https://reports.example.test", Mode: "fuzzy"}, want: "url assertion mode must be exact, prefix or regex"},
		{name: "status out of range", assertion: Assertion{Kind: "http_status", Status: 7}, want: "http_status assertion requires status between 100 and 599"},
		{name: "element_count without a target", assertion: Assertion{Kind: "element_count", Count: intPointer(1)}, want: "assertion requires a semantic target"},
		{name: "element_count without a bound", assertion: Assertion{Kind: "element_count", Target: &Target{Role: "row", Name: "Invoice"}}, want: "element_count assertion requires count, min or max"},
		{name: "element_state with an unknown state", assertion: Assertion{Kind: "element_state", Target: &Target{Role: "button", Name: "Export"}, State: "sleepy"}, want: "element_state assertion state must be enabled, editable, checked or focused"},
		{name: "attribute without a name", assertion: Assertion{Kind: "attribute", Target: &Target{Role: "button", Name: "Export"}, Expected: "false"}, want: "attribute assertion requires attribute"},
		{name: "download without a filename", assertion: Assertion{Kind: "download", SHA256: digest}, want: "download assertion requires filename"},
		{name: "download without an expectation", assertion: Assertion{Kind: "download", Filename: "export.csv"}, want: "download assertion requires sha256 or bytes"},
		{name: "download with a short digest", assertion: Assertion{Kind: "download", Filename: "export.csv", SHA256: "abc"}, want: "download assertion sha256 must be 64 hex characters"},
		{name: "url carrying a target", assertion: Assertion{Kind: "url", Expected: "https://reports.example.test", Target: &Target{Role: "button", Name: "Export"}}, want: "target is not valid for this assertion kind"},
		{name: "http_status carrying count", assertion: Assertion{Kind: "http_status", Status: 200, Count: intPointer(2)}, want: "count/min/max are only valid for element_count"},
		{name: "attribute carrying state", assertion: Assertion{Kind: "attribute", Target: &Target{Role: "button", Name: "Export"}, Attribute: "aria-disabled", Expected: "false", State: "enabled"}, want: "state/negate are only valid for element_state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := validRecipe("https://reports.example.test")
			value.Inputs = nil
			value.Steps = []Step{{ID: "check", Action: "assert", Assert: &tt.assertion}}
			err := Validate(value)
			if err == nil {
				t.Fatalf("Validate accepted %+v", tt.assertion)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestRecipeSchemaRejectsAssertOnOtherActions(t *testing.T) {
	value := validRecipe("https://reports.example.test")
	value.Inputs = nil
	value.Steps = []Step{{
		ID: "wait", Action: "timer", TimerMS: 10,
		Assert: &Assertion{Kind: "http_status", Status: 200},
	}}
	err := Validate(value)
	if err == nil || !strings.Contains(err.Error(), "assert is only valid for assert") {
		t.Fatalf("Validate = %v, want it to reject an assertion on a timer step", err)
	}
}

// assertingSurface is a fakeSurface that also implements the optional Asserter
// capability, which is how a recipe assert step reaches the browser.
type assertingSurface struct {
	*fakeSurface
	mu   sync.Mutex
	seen []Assertion
	err  error
}

func (a *assertingSurface) Assert(_ context.Context, assertion Assertion) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, assertion)
	return a.err
}

func TestRunnerRunsAssertStepsThroughTheSurface(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	value.Inputs = map[string]Input{"digest": {Required: true}}
	value.Steps = []Step{
		{ID: "status_ok", Action: "assert", Assert: &Assertion{Kind: "http_status", Status: 200}},
		{ID: "file_ok", Action: "assert", Assert: &Assertion{Kind: "download", Filename: "invoices.zip", SHA256: "${input:digest}"}},
	}
	digest := strings.Repeat("ab", 32)
	surface := &assertingSurface{fakeSurface: newFakeSurface()}

	result, err := Runner{Surface: surface}.Run(context.Background(), value, map[string]string{"digest": digest})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != "done" || len(result.Steps) != 2 {
		t.Fatalf("run result = %+v", result)
	}
	if len(surface.seen) != 2 {
		t.Fatalf("surface saw %d assertions, want 2", len(surface.seen))
	}
	if surface.seen[1].SHA256 != digest {
		t.Fatalf("sha256 = %q, want the expanded input %q", surface.seen[1].SHA256, digest)
	}
}

func TestRunnerFailsTheRunOnAFailedAssertion(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	value.Inputs = nil
	value.Steps = []Step{{ID: "status_ok", Action: "assert", Assert: &Assertion{Kind: "http_status", Status: 200}}}
	surface := &assertingSurface{
		fakeSurface: newFakeSurface(),
		err:         errSurfaceAssertionFailure,
	}

	result, err := Runner{Surface: surface}.Run(context.Background(), value, nil)
	if err == nil {
		t.Fatal("a failed assertion did not fail the run")
	}
	if !strings.Contains(err.Error(), "http status assertion failed: expected 200, actual 404") {
		t.Fatalf("error = %q, want the assertion message to survive", err.Error())
	}
	if result.Status != "failed" {
		t.Fatalf("run status = %q, want failed", result.Status)
	}
}

// TestRunnerReportsAMissingAsserterByName pins the capability contract: a
// surface that cannot assert says so instead of quietly passing the step.
func TestRunnerReportsAMissingAsserterByName(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	value.Inputs = nil
	value.Steps = []Step{{ID: "status_ok", Action: "assert", Assert: &Assertion{Kind: "http_status", Status: 200}}}

	_, err := Runner{Surface: surfaceWithoutEventArmer{newFakeSurface()}}.Run(context.Background(), value, nil)
	if err == nil || !strings.Contains(err.Error(), "deterministic assertions are unavailable on this browser surface") {
		t.Fatalf("error = %v, want a named capability failure", err)
	}
}

// TestBrowserSurfaceAssertCountsSemanticTargets proves the recipe's element
// assertions resolve a semantic target rather than carrying an ephemeral ref,
// and that the failure text matches the tool surface's wording.
func TestBrowserSurfaceAssertCountsSemanticTargets(t *testing.T) {
	controller := &findOnlyController{result: snapshot.FindResult{Elements: []snapshot.Element{
		{Ref: "e1", Role: "row", Name: "Invoice 1", Visible: true},
		{Ref: "e2", Role: "row", Name: "Invoice 2", Visible: true},
		{Ref: "e3", Role: "button", Name: "Export", Visible: true},
	}}}
	surface := &BrowserSurface{Browser: controller}
	target := &Target{Role: "row", NameContains: "Invoice"}

	if err := surface.Assert(context.Background(), Assertion{
		Kind: browser.AssertionElementCount, Target: target, Count: intPointer(2),
	}); err != nil {
		t.Fatalf("matching count: %v", err)
	}
	err := surface.Assert(context.Background(), Assertion{
		Kind: browser.AssertionElementCount, Target: target, Min: intPointer(5),
	})
	if err == nil {
		t.Fatal("count assertion passed with only two matches")
	}
	want := `element count assertion failed: expected at least 5 elements matching role "row" named "Invoice", actual 2`
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	if controller.opts.Role != "row" {
		t.Fatalf("find options = %+v, want the semantic role forwarded", controller.opts)
	}
}

func TestBrowserSurfaceAssertRefusesAnAmbiguousTarget(t *testing.T) {
	controller := &findOnlyController{result: snapshot.FindResult{Elements: []snapshot.Element{
		{Ref: "e1", Role: "button", Name: "Export", Visible: true},
		{Ref: "e2", Role: "button", Name: "Export", Visible: true},
	}}}
	surface := &BrowserSurface{Browser: controller}
	err := surface.Assert(context.Background(), Assertion{
		Kind: browser.AssertionElementState, Target: &Target{Role: "button", Name: "Export"}, State: "enabled",
	})
	if err == nil || !strings.Contains(err.Error(), "resolved to 2 elements; refusing to guess") {
		t.Fatalf("error = %v, want an ambiguity refusal", err)
	}
}

func intPointer(value int) *int { return &value }

// errSurfaceAssertionFailure stands in for whatever the surface reports. The
// production wording of each kind is pinned in internal/browser against the real
// formatter; this file only proves the runner carries a failure out intact.
var errSurfaceAssertionFailure = errors.New("http status assertion failed: expected 200, actual 404")

// TestBrowserSurfaceDownloadAssertionKeepsTheEntryForALaterCapture pins the
// ledger read a download assertion makes when no postcondition cached the entry.
// Downloads() is delta-scoped inside a recipe run: reading it consumes this
// tab's window, so an assertion that read and discarded would leave a following
// capture step with nothing to capture.
func TestBrowserSurfaceDownloadAssertionKeepsTheEntryForALaterCapture(t *testing.T) {
	payload := []byte("invoice bytes")
	path := filepath.Join(t.TempDir(), "invoice.pdf")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	entry := browser.DownloadEntry{GUID: "download-1", SuggestedFilename: "invoice.pdf", State: "completed", Path: path}
	controller := &downloadOnlyController{results: []browser.DownloadsResult{
		{Supported: true, Downloads: []browser.DownloadEntry{entry}},
		{Supported: true},
	}}
	artifacts := &completedDownloadArtifacts{}
	surface := &BrowserSurface{Browser: controller, Artifacts: artifacts}
	ctx := browser.WithTabID(context.Background(), "tab-7")

	size := int64(len(payload))
	if err := surface.Assert(ctx, Assertion{
		Kind: browser.AssertionDownload, Filename: "invoice.pdf",
		SHA256: hex.EncodeToString(digest[:]), Bytes: &size,
	}); err != nil {
		t.Fatalf("download assertion: %v", err)
	}

	meta, err := surface.Capture(ctx, CaptureSpec{Kind: "download", Filename: "invoice.pdf"})
	if err != nil {
		t.Fatalf("capture after assertion: %v", err)
	}
	if artifacts.completed.GUID != "download-1" || meta.ID == "" {
		t.Fatalf("capture saw %+v (meta %+v), want the asserted download", artifacts.completed, meta)
	}
	if artifacts.directCalls != 0 {
		t.Fatalf("capture fell through to a live artifact capture %d time(s); the assertion consumed the ledger entry", artifacts.directCalls)
	}
}

// TestBrowserSurfaceDownloadAssertionNamesAnUnsupportedTransport keeps the
// capability failure named on the recipe path too, in the words the tool surface
// uses.
func TestBrowserSurfaceDownloadAssertionNamesAnUnsupportedTransport(t *testing.T) {
	controller := &downloadOnlyController{results: []browser.DownloadsResult{
		{Supported: false, Note: "the extension bridge cannot observe downloads"},
	}}
	surface := &BrowserSurface{Browser: controller}
	size := int64(4)
	err := surface.Assert(browser.WithTabID(context.Background(), "tab-7"), Assertion{
		Kind: browser.AssertionDownload, Filename: "invoice.pdf", Bytes: &size,
	})
	want := "download digest assertions are unavailable on this transport: the extension bridge cannot observe downloads"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

// TestBrowserSurfaceAssertRejectsAMissingTarget covers the exported entry point
// directly: Assert satisfies the exported Asserter interface, so it cannot
// assume a Runner validated the step first.
func TestBrowserSurfaceAssertRejectsAMissingTarget(t *testing.T) {
	surface := &BrowserSurface{Browser: &findOnlyController{}}
	tests := []struct {
		name      string
		assertion Assertion
		want      string
	}{
		{
			name:      "element_count",
			assertion: Assertion{Kind: browser.AssertionElementCount, Count: intPointer(1)},
			want:      "element_count assertion requires a semantic target",
		},
		{
			name:      "element_state",
			assertion: Assertion{Kind: browser.AssertionElementState, State: browser.AssertStateEnabled},
			want:      "element_state assertion requires a semantic target",
		},
		{
			name:      "attribute",
			assertion: Assertion{Kind: browser.AssertionAttribute, Attribute: "aria-disabled", Expected: "false"},
			want:      "attribute assertion requires a semantic target",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := surface.Assert(context.Background(), tt.assertion)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestBrowserSurfaceCountReportsTruncationAsACountingFailure pins the wording a
// recipe author sees when the target matches more elements than the surface
// resolves: "refine the target before acting" alone does not read as an answer
// to a count.
func TestBrowserSurfaceCountReportsTruncationAsACountingFailure(t *testing.T) {
	controller := &findOnlyController{result: snapshot.FindResult{
		Elements: []snapshot.Element{{Ref: "e1", Role: "row", Name: "Invoice 1", Visible: true}},
		Metadata: map[string]any{"truncated": true},
	}}
	surface := &BrowserSurface{Browser: controller}
	err := surface.Assert(context.Background(), Assertion{
		Kind: browser.AssertionElementCount, Target: &Target{Role: "row"}, Min: intPointer(1),
	})
	want := "element_count assertion cannot count past 200 matching elements: semantic target search was truncated; refine the recipe target before acting"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}
