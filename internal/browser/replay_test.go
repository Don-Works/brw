package browser

import (
	"strings"
	"testing"
)

func boolp(v bool) *bool { return &v }

func recordedFlow() TraceResult {
	return TraceResult{Entries: []TraceEntry{
		{Action: "navigate_to", Text: "https://example.com/form", OK: true},
		{Action: "fill", Ref: "e1", Text: "ada@example.com", Name: "Email", Role: "textbox", OK: true},
		{Action: "select", Ref: "e3", Value: "uk", Name: "Country", Role: "combobox", OK: true},
		{Action: "click", Ref: "e4", Name: "Submit", Role: "button", OK: true},
	}}
}

func actionsOnly(steps []BatchStep) []string {
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, step.Action)
	}
	return out
}

func TestTraceToBatchReproducesTheFlowInOrder(t *testing.T) {
	got := TraceToBatch(recordedFlow(), ReplayOptions{Guards: boolp(false)})

	want := []string{"navigate_to", "fill", "select", "click"}
	if strings.Join(actionsOnly(got.Steps), ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", actionsOnly(got.Steps), want)
	}
	if got.Actions != 4 || got.Guards != 0 {
		t.Fatalf("actions=%d guards=%d, want 4/0 with guards off", got.Actions, got.Guards)
	}
	if got.Steps[0].URL != "https://example.com/form" {
		t.Errorf("navigate_to lost its URL: %+v", got.Steps[0])
	}
	if got.Steps[1].Text != "ada@example.com" {
		t.Errorf("fill lost its text: %+v", got.Steps[1])
	}
	if got.Steps[2].Value != "uk" {
		t.Errorf("select lost its value: %+v", got.Steps[2])
	}
}

// A ref only means something against the page that produced it. Replaying one
// blind can act on whatever inherited it, so a text-bearing element gets an
// identity check ahead of the action.
func TestTraceToBatchGuardsTextBearingElements(t *testing.T) {
	got := TraceToBatch(recordedFlow(), ReplayOptions{})

	want := []string{"navigate_to", "fill", "select", "assert_text", "click"}
	if strings.Join(actionsOnly(got.Steps), ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", actionsOnly(got.Steps), want)
	}
	guard := got.Steps[3]
	if guard.Ref != "e4" || guard.Text != "Submit" {
		t.Fatalf("guard = %+v, want an assert_text on e4 for Submit", guard)
	}
	if got.Guards != 1 {
		t.Fatalf("guards = %d, want 1", got.Guards)
	}
}

// assert_text compares innerText/textContent/value. A textbox labelled by
// aria-label has none of those, so guarding it would assert against an empty
// string and fail every replay. Those actions go unguarded, and say so.
func TestTraceToBatchReportsUnguardableActions(t *testing.T) {
	got := TraceToBatch(recordedFlow(), ReplayOptions{})

	if len(got.Unguarded) != 2 {
		t.Fatalf("unguarded = %v, want the textbox and the combobox", got.Unguarded)
	}
	joined := strings.Join(got.Unguarded, " ")
	if !strings.Contains(joined, "e1") || !strings.Contains(joined, "e3") {
		t.Errorf("unguarded does not name the refs it could not check: %v", got.Unguarded)
	}
	if !strings.Contains(got.Note, "ref alone") {
		t.Errorf("note does not warn about unguarded steps: %q", got.Note)
	}
}

// Coordinates recorded against one layout land somewhere else after a reflow.
// Exporting them as if they were replayable would be worse than skipping them.
func TestTraceToBatchSkipsCoordinateActionsAndSaysWhy(t *testing.T) {
	trace := TraceResult{Entries: []TraceEntry{
		{Action: "click", Ref: "e1", Name: "Go", Role: "button", OK: true},
		{Action: "drag", OK: true},
		{Action: "click_button", OK: true},
		{Action: "navigate", Text: "back", OK: true},
	}}
	got := TraceToBatch(trace, ReplayOptions{Guards: boolp(false)})

	if got.Actions != 1 {
		t.Fatalf("actions = %d, want only the ref click", got.Actions)
	}
	if got.Skipped != 3 {
		t.Fatalf("skipped = %d, want 3", got.Skipped)
	}
	var sawCoordinate, sawHistory bool
	for reason := range got.Reasons {
		if strings.Contains(reason, "coordinate-driven") {
			sawCoordinate = true
		}
		if strings.Contains(reason, "history navigation") {
			sawHistory = true
		}
	}
	if !sawCoordinate || !sawHistory {
		t.Fatalf("skip reasons do not explain what was dropped: %v", got.Reasons)
	}
}

func TestTraceToBatchFailedStepHandling(t *testing.T) {
	trace := TraceResult{Entries: []TraceEntry{
		{Action: "click", Ref: "e1", Name: "Works", Role: "button", OK: true},
		{Action: "click", Ref: "e2", Name: "Broken", Role: "button", OK: false},
	}}

	// Faithful by default: the export is what the agent did, with a warning.
	kept := TraceToBatch(trace, ReplayOptions{Guards: boolp(false)})
	if kept.Actions != 2 || kept.Failed != 1 {
		t.Fatalf("actions=%d failed=%d, want 2/1", kept.Actions, kept.Failed)
	}
	if !strings.Contains(kept.Note, "failed when recorded") {
		t.Errorf("note does not flag the failed step: %q", kept.Note)
	}

	dropped := TraceToBatch(trace, ReplayOptions{Guards: boolp(false), IncludeFailed: boolp(false)})
	if dropped.Actions != 1 || dropped.Failed != 0 {
		t.Fatalf("actions=%d failed=%d, want only the successful step", dropped.Actions, dropped.Failed)
	}
}

func TestTraceToBatchEmptyTraceExplainsItself(t *testing.T) {
	got := TraceToBatch(TraceResult{}, ReplayOptions{})
	if got.Count != 0 {
		t.Fatalf("count = %d, want 0", got.Count)
	}
	if !strings.Contains(got.Note, "no replayable actions") {
		t.Fatalf("empty trace note = %q", got.Note)
	}
	// An empty slice, not nil, so the payload carries [] rather than null.
	if got.Steps == nil {
		t.Fatal("steps is nil; it would marshal as null")
	}
}

func TestTraceToBatchWarnsWhenGuardsAreOff(t *testing.T) {
	got := TraceToBatch(recordedFlow(), ReplayOptions{Guards: boolp(false)})
	if !strings.Contains(got.Note, "guards disabled") {
		t.Fatalf("disabling guards is not surfaced: %q", got.Note)
	}
}

func TestTraceToBatchDropsActionsWithNoTarget(t *testing.T) {
	trace := TraceResult{Entries: []TraceEntry{
		{Action: "click", OK: true},  // no ref
		{Action: "press", OK: true},  // no key
		{Action: "select", OK: true}, // no ref
	}}
	got := TraceToBatch(trace, ReplayOptions{})
	if got.Actions != 0 || got.Skipped != 3 {
		t.Fatalf("actions=%d skipped=%d, want 0/3", got.Actions, got.Skipped)
	}
}
