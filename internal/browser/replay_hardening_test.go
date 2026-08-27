package browser

import (
	"strings"
	"testing"
)

// A fill whose value was never recorded must not export as a fill with empty
// text: both executors accept that and CLEAR the field, so a replay would
// destroy data rather than reproduce it.
func TestReplayNeverExportsAnEmptyValue(t *testing.T) {
	trace := TraceResult{Entries: []TraceEntry{
		{Action: "fill", Ref: "e1", Name: "Email", Role: "textbox", OK: true},      // no text
		{Action: "type", Ref: "e2", Name: "Name", Role: "textbox", OK: true},       // no text
		{Action: "select", Ref: "e3", Name: "Country", Role: "combobox", OK: true}, // no value
	}}
	got := TraceToBatch(trace, ReplayOptions{})

	if got.Actions != 0 {
		t.Fatalf("exported %d actions from entries with no values: %+v", got.Actions, got.Steps)
	}
	if got.Skipped != 3 {
		t.Fatalf("skipped = %d, want 3", got.Skipped)
	}
	var explained bool
	for reason := range got.Reasons {
		if strings.Contains(reason, "did not capture") {
			explained = true
		}
	}
	if !explained {
		t.Fatalf("skip reasons do not say the value was missing: %v", got.Reasons)
	}
}

// A credential-bearing field is recorded as an action but never with its value,
// and the export says so rather than pretending the step is reproducible.
func TestReplayReportsWithheldCredentials(t *testing.T) {
	trace := TraceResult{Entries: []TraceEntry{
		{Action: "fill", Ref: "e1", Name: "Password", Role: "textbox", OK: true, Redacted: true},
	}}
	got := TraceToBatch(trace, ReplayOptions{})

	if got.Actions != 0 {
		t.Fatalf("a redacted fill was exported: %+v", got.Steps)
	}
	var said bool
	for reason := range got.Reasons {
		if strings.Contains(reason, "credential-bearing") {
			said = true
		}
	}
	if !said {
		t.Fatalf("skip reasons do not explain the redaction: %v", got.Reasons)
	}
}

// A ref only means something inside the tab that issued it. A flow crossing
// tabs must carry explicit focus_tab steps, or every step runs in whichever tab
// the batch starts in — silently, since a textbox carries no guard.
func TestReplayEmitsFocusTabWhenTheFlowCrossesTabs(t *testing.T) {
	trace := TraceResult{Entries: []TraceEntry{
		{Action: "fill", Ref: "e1", Text: "one", TabID: "tab-A", OK: true},
		{Action: "fill", Ref: "e1", Text: "two", TabID: "tab-B", OK: true},
		{Action: "click", Ref: "e2", TabID: "tab-B", OK: true},
	}}
	got := TraceToBatch(trace, ReplayOptions{Guards: boolp(false)})

	actions := actionsOnly(got.Steps)
	want := []string{"fill", "focus_tab", "fill", "click"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", actions, want)
	}
	if got.TabSwitches != 1 {
		t.Fatalf("tab switches = %d, want 1", got.TabSwitches)
	}
	for _, step := range got.Steps {
		if step.Action == "focus_tab" && step.ID != "tab-B" {
			t.Fatalf("focus_tab targets %q, want tab-B", step.ID)
		}
	}
}

func TestReplaySingleTabFlowNeedsNoFocusStep(t *testing.T) {
	trace := TraceResult{Entries: []TraceEntry{
		{Action: "fill", Ref: "e1", Text: "one", TabID: "tab-A", OK: true},
		{Action: "click", Ref: "e2", TabID: "tab-A", OK: true},
	}}
	got := TraceToBatch(trace, ReplayOptions{Guards: boolp(false)})
	if got.TabSwitches != 0 {
		t.Fatalf("a single-tab flow emitted %d focus_tab steps", got.TabSwitches)
	}
}

// assert_text reads innerText, textContent then value. An icon-only button
// carries its name in aria-label alone, so a guard on it could never pass and
// would fail every replay of an unchanged page.
func TestReplayGuardsOnlyWhenTheNameIsVisibleText(t *testing.T) {
	iconOnly := TraceResult{Entries: []TraceEntry{
		{Action: "click", Ref: "e1", Name: "Save", Role: "button", OK: true},
	}}
	got := TraceToBatch(iconOnly, ReplayOptions{})
	if got.Guards != 0 {
		t.Fatalf("guarded a button whose name is not visible text: %+v", got.Steps)
	}
	if len(got.Unguarded) != 1 {
		t.Fatalf("unguarded = %v, want the icon-only button reported", got.Unguarded)
	}

	labelled := TraceResult{Entries: []TraceEntry{
		{Action: "click", Ref: "e1", Name: "Save", Role: "button", NameIsVisibleText: true, OK: true},
	}}
	if guarded := TraceToBatch(labelled, ReplayOptions{}); guarded.Guards != 1 {
		t.Fatalf("guards = %d, want 1 for a button whose text is its name", guarded.Guards)
	}
}
