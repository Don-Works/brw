package snapshot

import (
	"encoding/json"
	"strings"
	"testing"
)

func boolp(b bool) *bool { return &b }

func TestRenderCompactShapeAndTokenSavings(t *testing.T) {
	snap := PageSnapshot{
		URL:   "https://shop.example.com/cart",
		Title: "Cart",
		Elements: []Element{
			{Ref: "e1", Role: "button", Name: "Place order", Tag: "button", Visible: true, InViewport: true, Source: []string{"dom"}},
			{Ref: "e2", Role: "textbox", Name: "Email", Tag: "input", Type: "email", Value: "you@example.com", Required: true, Visible: true, InViewport: true, Source: []string{"dom"}},
			{Ref: "e3", Role: "textbox", Name: "Password", Tag: "input", Type: "password", Value: "hunter2", Sensitive: true, Visible: true, InViewport: true, Source: []string{"dom"}},
			{Ref: "e4", Role: "checkbox", Name: "Agree", Tag: "input", Checked: boolp(true), Visible: true, InViewport: true, Source: []string{"dom"}},
			{Ref: "e5", Role: "link", Name: "Docs", Tag: "a", Href: "/docs", Visible: true, InViewport: true, Source: []string{"dom"}},
			{Ref: "e6", Role: "image", Name: "", Tag: "canvas", VisualType: "canvas", VisualHint: "sales chart", Visible: true, InViewport: true, Source: []string{"visual"}},
			{Ref: "e7", Role: "button", Name: "Disabled", Tag: "button", Disabled: true, Visible: true, InViewport: false, Source: []string{"dom"}},
		},
		Metadata: map[string]any{
			"element_count":    7,
			"total_candidates": 20,
			"version":          float64(4),
			"mode":             "frontier",
		},
	}

	out := RenderCompact(snap)

	wantLines := []string{
		`e1 button "Place order"`,
		`type=email`,
		`=you@example.com`,
		`required`,
		`=***`, // sensitive value masked
		`e4 checkbox "Agree" checked`,
		`e5 link "Docs" ->/docs`,
		`visual:canvas "sales chart"`,
		`disabled`,
		`offscreen`, // e7 is visible but not in viewport
		`7/20 controls`,
		`version 4`,
		`frontier`,
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("compact output missing %q\n--- got ---\n%s", w, out)
		}
	}

	// The password plaintext must never appear.
	if strings.Contains(out, "hunter2") {
		t.Fatalf("sensitive value leaked into compact output:\n%s", out)
	}

	// Token-efficiency intent: compact must be materially smaller than the JSON
	// rendering of the same elements.
	jsonOut, _ := json.Marshal(snap.Elements)
	if len(out) >= len(jsonOut) {
		t.Fatalf("compact (%d bytes) should be smaller than JSON (%d bytes)", len(out), len(jsonOut))
	}
}

func TestRenderCompactDeltaHeader(t *testing.T) {
	snap := PageSnapshot{
		URL:      "https://example.com",
		Elements: []Element{{Ref: "e9", Role: "button", Name: "New", Tag: "button", Visible: true, InViewport: true, Source: []string{"dom"}}},
		Delta:    &SnapshotDelta{Added: []string{"e9"}, Changed: []string{}, Removed: []string{"e2"}},
		Metadata: map[string]any{"element_count": 1, "version": float64(8), "mode": "frontier"},
	}
	out := RenderCompact(snap)
	if !strings.Contains(out, "delta:") || !strings.Contains(out, "removed: e2") {
		t.Fatalf("expected delta header with removed refs, got:\n%s", out)
	}
}
