package browser

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestParseObserveLevel(t *testing.T) {
	tests := []struct {
		name         string
		value        string
		wantLevel    ObserveLevel
		wantExplicit bool
		wantErr      bool
	}{
		{name: "absent", value: "", wantLevel: ObserveFull},
		{name: "full", value: "full", wantLevel: ObserveFull, wantExplicit: true},
		{name: "minimal", value: "minimal", wantLevel: ObserveMinimal, wantExplicit: true},
		{name: "none", value: "none", wantLevel: ObserveNone, wantExplicit: true},
		{name: "case and space", value: "  MINIMAL ", wantLevel: ObserveMinimal, wantExplicit: true},
		// A value brw does not understand must be refused, not silently widened:
		// a caller who asked for fewer tokens and got all of them cannot tell.
		{name: "unknown", value: "summary", wantErr: true},
		{name: "off is not a level", value: "off", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level, explicit, err := ParseObserveLevel(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseObserveLevel(%q) = (%q, %v, nil), want an error", tt.value, level, explicit)
				}
				if !strings.Contains(err.Error(), "full, minimal, none") {
					t.Fatalf("error %q does not name the accepted values", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseObserveLevel(%q) = %v", tt.value, err)
			}
			if level != tt.wantLevel || explicit != tt.wantExplicit {
				t.Fatalf("ParseObserveLevel(%q) = (%q, %v), want (%q, %v)", tt.value, level, explicit, tt.wantLevel, tt.wantExplicit)
			}
		})
	}
}

func richActionResult() ActionResult {
	changed := true
	return ActionResult{
		OK:           true,
		Message:      "clicked e4",
		Warning:      "shift is still held",
		TabID:        "tab1",
		NewTabID:     "tab2",
		Version:      7,
		URL:          "https://fixture.test/cart",
		Title:        "Cart",
		Focus:        "e9",
		ChangedState: &changed,
		Targets:      []Tab{{ID: "tab1", URL: "https://fixture.test/cart"}},
		Changed:      []string{`e9 button "Checkout"`},
		Elements:     []snapshot.Element{{Ref: "e9", Role: "button", Name: "Checkout"}},
		Snapshot:     &snapshot.PageSnapshot{URL: "https://fixture.test/cart"},
		DurationMS:   42,
	}
}

func TestApplyToActionKeepsTheOutcomeAtEveryLevel(t *testing.T) {
	tests := []struct {
		name         string
		level        ObserveLevel
		wantElements bool
		wantTargets  bool
		wantSnapshot bool
		wantChanged  bool
		wantURL      bool
	}{
		{name: "full is unchanged", level: ObserveFull, wantElements: true, wantTargets: true, wantSnapshot: true, wantChanged: true, wantURL: true},
		{name: "minimal drops the element list", level: ObserveMinimal, wantChanged: true, wantURL: true},
		{name: "none is outcome only", level: ObserveNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.level.ApplyToAction(richActionResult())

			// The outcome survives every level. An action whose result cannot say
			// whether it worked is not a saving.
			if !got.OK || got.Message == "" || got.Warning == "" || got.TabID == "" || got.NewTabID == "" || got.DurationMS == 0 {
				t.Fatalf("level %q lost part of the outcome: %+v", tt.level, got)
			}
			if got.ChangedState == nil || !*got.ChangedState {
				t.Fatalf("level %q dropped changed_state, so the caller cannot tell the action did anything", tt.level)
			}

			if (len(got.Elements) > 0) != tt.wantElements {
				t.Fatalf("level %q elements = %v, want present=%v", tt.level, got.Elements, tt.wantElements)
			}
			if (len(got.Targets) > 0) != tt.wantTargets {
				t.Fatalf("level %q targets = %v, want present=%v", tt.level, got.Targets, tt.wantTargets)
			}
			if (got.Snapshot != nil) != tt.wantSnapshot {
				t.Fatalf("level %q snapshot present = %v, want %v", tt.level, got.Snapshot != nil, tt.wantSnapshot)
			}
			if (len(got.Changed) > 0) != tt.wantChanged {
				t.Fatalf("level %q changed = %v, want present=%v", tt.level, got.Changed, tt.wantChanged)
			}
			if (got.URL != "") != tt.wantURL || (got.Title != "") != tt.wantURL {
				t.Fatalf("level %q url/title = %q/%q, want present=%v", tt.level, got.URL, got.Title, tt.wantURL)
			}
		})
	}
}

// The default must be a no-op on the whole struct, not just on the fields the
// test above names: that is what "absent means byte-identical" rests on.
func TestObserveFullChangesNothing(t *testing.T) {
	input := richActionResult()
	if got := ObserveFull.ApplyToAction(input); !reflect.DeepEqual(got, input) {
		t.Fatalf("ObserveFull.ApplyToAction changed the result:\n got %+v\nwant %+v", got, input)
	}
	batch := BatchResult{OK: true, URL: "https://fixture.test/", Title: "T", Focus: "e1", Version: 3,
		Changed: []string{"e1"}, Steps: []BatchStepResult{{Index: 0, Action: "click", OK: true}}}
	if got := ObserveFull.ApplyToBatch(batch); !reflect.DeepEqual(got, batch) {
		t.Fatalf("ObserveFull.ApplyToBatch changed the result:\n got %+v\nwant %+v", got, batch)
	}
}

// A batch's per-step record says which step failed. Trimming the closing
// observation must never trim that, at any level.
func TestApplyToBatchKeepsTheStepRecord(t *testing.T) {
	batch := BatchResult{
		OK:             false,
		Error:          "step 1 failed",
		StepsCompleted: 1,
		URL:            "https://fixture.test/",
		Title:          "T",
		Changed:        []string{"e1"},
		Steps: []BatchStepResult{
			{Index: 0, Action: "click", OK: true, Ref: "e4"},
			{Index: 1, Action: "fill", OK: false, Error: "ref not found"},
		},
	}
	for _, level := range []ObserveLevel{ObserveFull, ObserveMinimal, ObserveNone} {
		got := level.ApplyToBatch(batch)
		if len(got.Steps) != 2 || got.Steps[1].Error != "ref not found" || got.Error != "step 1 failed" || got.StepsCompleted != 1 {
			t.Fatalf("level %q lost the step record: %+v", level, got)
		}
	}
	if got := ObserveNone.ApplyToBatch(batch); got.URL != "" || got.Changed != nil {
		t.Fatalf("ObserveNone kept the closing observation: %+v", got)
	}
	if got := ObserveMinimal.ApplyToBatch(batch); got.URL == "" || len(got.Changed) == 0 {
		t.Fatalf("ObserveMinimal dropped url/changed, which it must keep: %+v", got)
	}
}

func TestPlanStepObserveLevel(t *testing.T) {
	tests := []struct {
		name     string
		level    ObserveLevel
		explicit bool
		index    int
		total    int
		want     ObserveLevel
	}{
		{name: "default intermediate", level: ObserveFull, index: 0, total: 3, want: ObserveMinimal},
		{name: "default last", level: ObserveFull, index: 2, total: 3, want: ObserveFull},
		{name: "default single step is last", level: ObserveFull, index: 0, total: 1, want: ObserveFull},
		{name: "explicit none applies to the last step too", level: ObserveNone, explicit: true, index: 2, total: 3, want: ObserveNone},
		{name: "explicit full restores intermediates", level: ObserveFull, explicit: true, index: 0, total: 3, want: ObserveFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanStepObserveLevel(tt.level, tt.explicit, tt.index, tt.total); got != tt.want {
				t.Fatalf("PlanStepObserveLevel(%q, %v, %d, %d) = %q, want %q", tt.level, tt.explicit, tt.index, tt.total, got, tt.want)
			}
		})
	}
}

// A plan step result arrives as a typed ActionResult in-process and as a decoded
// object over the upstream HTTP transport. Both must trim to the same shape, or
// the same tool call answers differently depending on the topology.
func TestApplyToPlanTrimsBothStepResultShapes(t *testing.T) {
	decoded := func(result ActionResult) map[string]any {
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	typed := PlanResult{OK: true, Steps: []PlanStepResult{
		{Index: 0, Action: "click", OK: true, Result: richActionResult()},
		{Index: 1, Action: "click", OK: true, Result: richActionResult()},
	}}
	overHTTP := PlanResult{OK: true, Steps: []PlanStepResult{
		{Index: 0, Action: "click", OK: true, Result: decoded(richActionResult())},
		{Index: 1, Action: "click", OK: true, Result: decoded(richActionResult())},
	}}

	gotTyped := ObserveFull.ApplyToPlan(typed, false)
	first, ok := gotTyped.Steps[0].Result.(ActionResult)
	if !ok {
		t.Fatalf("typed intermediate step result is %T", gotTyped.Steps[0].Result)
	}
	if len(first.Elements) != 0 {
		t.Fatalf("typed intermediate step kept its element list: %+v", first)
	}
	last, ok := gotTyped.Steps[1].Result.(ActionResult)
	if !ok || len(last.Elements) == 0 {
		t.Fatalf("typed last step lost its element list: %+v", gotTyped.Steps[1].Result)
	}

	gotHTTP := ObserveFull.ApplyToPlan(overHTTP, false)
	firstMap, ok := gotHTTP.Steps[0].Result.(map[string]any)
	if !ok {
		t.Fatalf("decoded intermediate step result is %T", gotHTTP.Steps[0].Result)
	}
	if _, present := firstMap["elements"]; present {
		t.Fatalf("decoded intermediate step kept its element list: %v", firstMap)
	}
	if _, present := firstMap["url"]; !present {
		t.Fatalf("decoded intermediate step dropped url, which minimal keeps: %v", firstMap)
	}
	lastMap, ok := gotHTTP.Steps[1].Result.(map[string]any)
	if !ok {
		t.Fatalf("decoded last step result is %T", gotHTTP.Steps[1].Result)
	}
	if _, present := lastMap["elements"]; !present {
		t.Fatalf("decoded last step lost its element list: %v", lastMap)
	}
}

// A plan carries payloads that are not observations at all — a read, a
// structured-data extraction. Trimming must leave those alone; dropping "url"
// from a read result would corrupt the answer rather than shrink it.
func TestApplyToPlanLeavesNonObservationPayloadsAlone(t *testing.T) {
	read := map[string]any{"url": "https://fixture.test/", "title": "Doc", "main": "prose", "headings": []any{"H"}}
	plan := PlanResult{OK: true, Steps: []PlanStepResult{
		{Index: 0, Action: "read", OK: true, Result: read},
		{Index: 1, Action: "click", OK: true, Result: richActionResult()},
	}}
	got := ObserveNone.ApplyToPlan(plan, true)
	first, ok := got.Steps[0].Result.(map[string]any)
	if !ok {
		t.Fatalf("read step result is %T", got.Steps[0].Result)
	}
	if first["url"] != "https://fixture.test/" || first["main"] != "prose" {
		t.Fatalf("a read payload was trimmed as if it were an observation: %v", first)
	}
}

// An explicit level applies to every step including the last, and none drops the
// snapshot a snapshot step produced.
func TestApplyToPlanHonoursAnExplicitLevel(t *testing.T) {
	plan := PlanResult{OK: true, Steps: []PlanStepResult{
		{Index: 0, Action: "click", OK: true, Result: richActionResult()},
		{Index: 1, Action: "click", OK: true, Result: richActionResult()},
	}}
	got := ObserveNone.ApplyToPlan(plan, true)
	for i, step := range got.Steps {
		result, ok := step.Result.(ActionResult)
		if !ok {
			t.Fatalf("step %d result is %T", i, step.Result)
		}
		if result.URL != "" || len(result.Changed) > 0 || len(result.Elements) > 0 {
			t.Fatalf("step %d kept observation fields under observe=none: %+v", i, result)
		}
		if !result.OK || result.Message == "" {
			t.Fatalf("step %d lost its outcome: %+v", i, result)
		}
	}
}
