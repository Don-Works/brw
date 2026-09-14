package browser

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/readability"
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
	if got := ObserveFull.ApplyToNavigation(input); !reflect.DeepEqual(got, input) {
		t.Fatalf("ObserveFull.ApplyToNavigation changed the result:\n got %+v\nwant %+v", got, input)
	}
	batch := BatchResult{OK: true, URL: "https://fixture.test/", Title: "T", Focus: "e1", Version: 3,
		Changed: []string{"e1"}, Steps: []BatchStepResult{{Index: 0, Action: "click", OK: true}}}
	if got := ObserveFull.ApplyToBatch(batch); !reflect.DeepEqual(got, batch) {
		t.Fatalf("ObserveFull.ApplyToBatch changed the result:\n got %+v\nwant %+v", got, batch)
	}
	snap := snapshot.PageSnapshot{URL: "https://fixture.test/cart"}
	plan := PlanResult{OK: true, StepsCompleted: 2, Steps: []PlanStepResult{
		{Index: 0, Action: "snapshot", OK: true, Result: snap, Snapshot: &snap},
		{Index: 1, Action: "click", OK: true, Result: richActionResult()},
	}}
	if got := ObserveFull.ApplyToPlan(plan, true); !reflect.DeepEqual(got, plan) {
		t.Fatalf("ObserveFull.ApplyToPlan changed the result:\n got %+v\nwant %+v", got, plan)
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

// An explicit level applies to every step's observation, the last one included.
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

// planStepFixtures is one representative result per classified plan step verb,
// in the shape that verb's runner produces. The table is checked against the
// classification both ways, so a verb can neither be classified without a
// fixture here nor trimmed on a shape nobody looked at.
func planStepFixtures() map[string]struct {
	result      any
	wantTrimmed bool
} {
	return map[string]struct {
		result      any
		wantTrimmed bool
	}{
		"click":      {result: richActionResult(), wantTrimmed: true},
		"click_text": {result: richActionResult(), wantTrimmed: true},
		"type":       {result: richActionResult(), wantTrimmed: true},
		"fill":       {result: richActionResult(), wantTrimmed: true},
		"select":     {result: richActionResult(), wantTrimmed: true},
		"press":      {result: richActionResult(), wantTrimmed: true},
		"scroll":     {result: richActionResult(), wantTrimmed: true},
		"hover":      {result: richActionResult(), wantTrimmed: true},
		// Shaped the way the primitive answers: a message written from the url
		// that was REQUESTED and a committed url that is somewhere else.
		"navigate_to": {result: navigationActionResult(), wantTrimmed: true},
		"find_act": {result: FindActResult{
			Matched: snapshot.Element{Ref: "e9", Role: "button", Name: "Checkout"},
			Action:  "click",
			Result:  richActionResult(),
		}, wantTrimmed: true},
		// A step's own product is what the caller wrote the step to get.
		"snapshot": {result: snapshot.PageSnapshot{
			URL: "https://fixture.test/cart", Title: "Cart",
			Elements: []snapshot.Element{{Ref: "e9", Role: "button", Name: "Checkout"}},
		}},
		"read":      {result: readability.PageRead{URL: "https://fixture.test/cart", Title: "Cart", Main: "prose"}},
		"open":      {result: OpenResult{Tab: Tab{ID: "tab2", URL: "https://fixture.test/cart", Title: "Cart"}}},
		"wait":      {result: map[string]any{"ok": true, "message": "wait matched ready", "condition": "ready"}},
		"focus_tab": {result: map[string]any{"ok": true, "message": "focused tab tab1", "tab_id": "tab1"}},
	}
}

// The trim is driven by a table keyed on the step verb, so the table has to be
// exhaustive: a verb it does not know reports in full, which is safe but is a
// silent hole in what observe claims to do. This checks the fixtures and the
// classification against each other in both directions, then checks each verb's
// payload actually trims (or survives) as its classification says.
func TestEveryClassifiedPlanStepVerbTrimsAsItsPayloadAllows(t *testing.T) {
	fixtures := planStepFixtures()
	for _, verb := range ClassifiedPlanStepVerbs() {
		if _, ok := fixtures[verb]; !ok {
			t.Errorf("plan step verb %q is classified for observe but has no fixture, so nothing checks how it trims", verb)
		}
	}
	for verb := range fixtures {
		if !PlanStepVerbIsClassified(verb) {
			t.Errorf("fixture names plan step verb %q, which the observe classification does not know", verb)
		}
	}
	if t.Failed() {
		return
	}

	for verb, tt := range fixtures {
		t.Run(verb, func(t *testing.T) {
			before := mustJSON(t, tt.result)
			plan := PlanResult{OK: true, Steps: []PlanStepResult{
				{Index: 0, Action: verb, OK: true, Result: tt.result},
			}}
			got := mustJSON(t, ObserveNone.ApplyToPlan(plan, true).Steps[0].Result)
			if tt.wantTrimmed {
				if strings.Contains(got, `"elements"`) {
					t.Fatalf("observe=none left the element list on a %q step: %s", verb, got)
				}
				if !strings.Contains(got, `"ok":true`) {
					t.Fatalf("observe=none left a %q step unable to report its outcome: %s", verb, got)
				}
				// A verb whose result is a navigation keeps the committed url at
				// every level, because that url IS its outcome and its message
				// names the one that was requested. Every other observation drops
				// it. Checked here for EVERY classified verb, so a sibling verb
				// that navigates cannot be added as a plain observation.
				if keptURL, wantURL := strings.Contains(got, `"url"`), IsNavigationAction(verb); keptURL != wantURL {
					if wantURL {
						t.Fatalf("observe=none dropped the committed url from a %q step while keeping a message that names the requested one: %s", verb, got)
					}
					t.Fatalf("observe=none left the url on a %q step, which is an observation and not a destination: %s", verb, got)
				}
				return
			}
			if got != before {
				t.Fatalf("observe=none changed what a %q step produced:\n got %s\nwant %s", verb, got, before)
			}
		})
	}
}

// navigationActionResult is what a navigation primitive answers with: the
// message is written from the url the caller ASKED for, and URL is the one the
// browser committed to after the redirect.
func navigationActionResult() ActionResult {
	result := richActionResult()
	result.Message = "navigated to https://fixture.test/cart"
	result.URL = "https://fixture.test/login?next=%2Fcart"
	return result
}

// The navigation exception is keyed on one set of verbs, not on a tool name and
// not on a step verb: brw_navigate_to and a brw_plan navigate_to step run the
// same primitive, and fixing the tool while leaving the step classified as a
// plain observation is how the step went on reporting a destination brw never
// verified. Both directions, over the whole domain of classified verbs.
func TestEveryNavigationVerbIsClassifiedAsOne(t *testing.T) {
	for _, verb := range ClassifiedPlanStepVerbs() {
		classified := PlanStepVerbKeepsTheCommittedURL(verb)
		if navigates := IsNavigationAction(verb); classified != navigates {
			if navigates {
				t.Errorf("%q is a navigation action but its plan step is not classified as one, so observe drops the url it did not verify", verb)
				continue
			}
			t.Errorf("plan step verb %q is classified as a navigation but is not a navigation action", verb)
		}
	}
	// The set itself has to be non-empty and reach the classification, or both
	// directions above are vacuously true.
	if len(NavigationActions()) == 0 {
		t.Fatal("NavigationActions() is empty, so nothing above checked anything")
	}
	classified := 0
	for _, verb := range NavigationActions() {
		if PlanStepVerbIsClassified(verb) {
			classified++
		}
	}
	if classified == 0 {
		t.Fatalf("no navigation action %v is a classified plan step verb, so the plan surface checks nothing", NavigationActions())
	}
}

// A navigate_to plan step arrives typed in-process and as a decoded object over
// the upstream HTTP proxy. Both drop the observation and both keep the url: the
// shape a step arrives in is not something the caller chose, and the proxy is
// the transport where a plan step is decoded rather than constructed.
func TestApplyToPlanKeepsTheCommittedURLOnANavigateStep(t *testing.T) {
	typedStep := navigationActionResult()
	var decodedStep map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, typedStep)), &decodedStep); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, level := range []ObserveLevel{ObserveFull, ObserveMinimal, ObserveNone} {
		t.Run(string(level), func(t *testing.T) {
			plan := PlanResult{OK: true, Steps: []PlanStepResult{
				{Index: 0, Action: "navigate_to", OK: true, Result: typedStep},
				{Index: 1, Action: "navigate_to", OK: true, Result: decodedStep},
			}}
			got := level.ApplyToPlan(plan, true)

			typedOut, ok := got.Steps[0].Result.(ActionResult)
			if !ok {
				t.Fatalf("typed navigate_to step result is %T", got.Steps[0].Result)
			}
			if typedOut.URL != typedStep.URL {
				t.Fatalf("level %q reported url %q on a typed navigate_to step while claiming %q",
					level, typedOut.URL, typedOut.Message)
			}
			decodedOut, ok := got.Steps[1].Result.(map[string]any)
			if !ok {
				t.Fatalf("decoded navigate_to step result is %T", got.Steps[1].Result)
			}
			if decodedOut["url"] != typedStep.URL {
				t.Fatalf("level %q reported url %v on a decoded navigate_to step while claiming %v",
					level, decodedOut["url"], decodedOut["message"])
			}
			if level == ObserveNone {
				// The exception is one field wide: everything else still trims, or
				// it is a way out of the parameter rather than a correction to it.
				if len(typedOut.Elements) != 0 || typedOut.Snapshot != nil || typedOut.Title != "" {
					t.Fatalf("observe=none left the observation on a typed navigate_to step: %+v", typedOut)
				}
				for _, key := range []string{"elements", "snapshot", "title", "changed"} {
					if _, present := decodedOut[key]; present {
						t.Fatalf("observe=none left %q on a decoded navigate_to step: %v", key, decodedOut)
					}
				}
			}
		})
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

// A find_act step is an observation wrapped in {matched, action, result}. It
// arrives typed in-process and decoded over the upstream HTTP transport, and
// both have to trim — the shape-sniffing predicate this replaced trimmed
// neither, so observe:"none" over two find_act steps returned two full element
// lists on both transports.
func TestApplyToPlanTrimsBothFindActShapes(t *testing.T) {
	typedStep := FindActResult{
		Matched: snapshot.Element{Ref: "e9", Role: "button", Name: "Checkout"},
		Action:  "click",
		Result:  richActionResult(),
	}
	var decodedStep map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, typedStep)), &decodedStep); err != nil {
		t.Fatalf("decode: %v", err)
	}

	plan := PlanResult{OK: true, Steps: []PlanStepResult{
		{Index: 0, Action: "find_act", OK: true, Result: typedStep},
		{Index: 1, Action: "find_act", OK: true, Result: decodedStep},
	}}
	got := ObserveNone.ApplyToPlan(plan, true)

	typedOut, ok := got.Steps[0].Result.(FindActResult)
	if !ok {
		t.Fatalf("typed find_act step result is %T", got.Steps[0].Result)
	}
	if len(typedOut.Result.Elements) != 0 || typedOut.Result.URL != "" || typedOut.Result.Snapshot != nil {
		t.Fatalf("observe=none left the observation on a typed find_act step: %+v", typedOut.Result)
	}
	if typedOut.Matched.Ref != "e9" || typedOut.Action != "click" {
		t.Fatalf("the matched element is the answer, not the observation, and was trimmed away: %+v", typedOut)
	}

	decodedOut, ok := got.Steps[1].Result.(map[string]any)
	if !ok {
		t.Fatalf("decoded find_act step result is %T", got.Steps[1].Result)
	}
	nested, ok := decodedOut["result"].(map[string]any)
	if !ok {
		t.Fatalf("decoded find_act step lost its result object: %v", decodedOut)
	}
	for _, key := range []string{"elements", "url", "snapshot"} {
		if _, present := nested[key]; present {
			t.Fatalf("observe=none left %q on a decoded find_act step: %v", key, nested)
		}
	}
	if _, present := decodedOut["matched"]; !present {
		t.Fatalf("decoded find_act step lost the element it acted on: %v", decodedOut)
	}
}

// The snapshot a `snapshot` step fetched is the reason the step exists, and
// SKILL.md sends agents to brw_plan for exactly that mid-flow snapshot. The
// default intermediate trim used to delete it, so the documented way to get one
// was a round trip that returned nothing.
func TestApplyToPlanKeepsAStepsOwnSnapshot(t *testing.T) {
	snap := snapshot.PageSnapshot{URL: "https://fixture.test/cart", Title: "Cart",
		Elements: []snapshot.Element{{Ref: "e9", Role: "button", Name: "Checkout"}}}

	for _, tt := range []struct {
		name     string
		level    ObserveLevel
		explicit bool
	}{
		{name: "default", level: ObserveFull},
		{name: "explicit minimal", level: ObserveMinimal, explicit: true},
		{name: "explicit none", level: ObserveNone, explicit: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := PlanResult{OK: true, Steps: []PlanStepResult{
				{Index: 0, Action: "snapshot", OK: true, Result: snap, Snapshot: &snap},
				{Index: 1, Action: "click", OK: true, Result: richActionResult()},
			}}
			got := tt.level.ApplyToPlan(plan, tt.explicit)
			if got.Steps[0].Snapshot == nil {
				t.Fatalf("level %q dropped the snapshot a snapshot step produced", tt.level)
			}
			if len(got.Steps[0].Snapshot.Elements) == 0 {
				t.Fatalf("level %q emptied the snapshot a snapshot step produced", tt.level)
			}
			if mustJSON(t, got.Steps[0].Result) != mustJSON(t, snap) {
				t.Fatalf("level %q trimmed a snapshot step's result: %s", tt.level, mustJSON(t, got.Steps[0].Result))
			}
		})
	}
}

// ApplyToPlan writes into copies. The caller's PlanResult shares the step slice
// and any decoded map inside it, so trimming in place would edit a result that
// may still be read or logged untrimmed — an aliasing bug that only shows up
// the first time a plan result is read twice.
func TestApplyToPlanDoesNotMutateItsInput(t *testing.T) {
	decoded := map[string]any{"ok": true, "message": "clicked e4", "url": "https://fixture.test/cart",
		"changed": []any{"e9"}, "elements": []any{map[string]any{"ref": "e9"}}}
	plan := PlanResult{OK: true, Steps: []PlanStepResult{
		{Index: 0, Action: "click", OK: true, Result: richActionResult()},
		{Index: 1, Action: "click", OK: true, Result: decoded},
	}}
	before := mustJSON(t, plan)

	trimmed := ObserveNone.ApplyToPlan(plan, true)
	if got := mustJSON(t, trimmed); got == before {
		t.Fatalf("ApplyToPlan trimmed nothing, so this proves nothing: %s", got)
	}
	if after := mustJSON(t, plan); after != before {
		t.Fatalf("ApplyToPlan modified the result it was given:\n got %s\nwant %s", after, before)
	}
	if _, present := decoded["elements"]; !present {
		t.Fatal("ApplyToPlan deleted a key from the caller's own map")
	}
}

// A navigation's outcome IS the destination, and the message is written from
// the url that was REQUESTED before the observation reads the committed one. A
// level that kept that message and dropped the url would report arriving
// somewhere brw never verified.
func TestApplyToNavigationKeepsTheCommittedURL(t *testing.T) {
	for _, level := range []ObserveLevel{ObserveFull, ObserveMinimal, ObserveNone} {
		result := richActionResult()
		result.Message = "navigated to https://fixture.test/login"
		result.URL = "https://fixture.test/login?next=%2Fcart"

		got := level.ApplyToNavigation(result)
		if got.URL != result.URL {
			t.Fatalf("level %q dropped the committed url while keeping %q, which claims a destination brw did not verify",
				level, got.Message)
		}
		if !got.OK || got.Message == "" {
			t.Fatalf("level %q lost the outcome: %+v", level, got)
		}
		// Everything else still trims, or the exception would be a way out of
		// the parameter rather than a correction to it.
		if level == ObserveNone && (len(got.Elements) > 0 || len(got.Changed) > 0 || got.Title != "") {
			t.Fatalf("ApplyToNavigation(none) kept more than the url: %+v", got)
		}
		if level == ObserveMinimal && len(got.Elements) > 0 {
			t.Fatalf("ApplyToNavigation(minimal) kept the element list: %+v", got)
		}
	}
}

// ObserveMinimal on a batch is the same as ObserveFull because a BatchResult
// has no element list to drop. That is worth pinning: it is the reason
// brw_batch advertises its own schema text instead of promising a saving it
// cannot make.
func TestApplyToBatchMinimalIsTheSameAsFull(t *testing.T) {
	batch := BatchResult{OK: true, TabID: "tab1", URL: "https://fixture.test/", Title: "T", Focus: "e1",
		Version: 3, Changed: []string{"e1"}, StepsCompleted: 1,
		Steps: []BatchStepResult{{Index: 0, Action: "click", OK: true, Ref: "e4"}}}
	if !reflect.DeepEqual(ObserveMinimal.ApplyToBatch(batch), ObserveFull.ApplyToBatch(batch)) {
		t.Fatal("ObserveMinimal.ApplyToBatch now differs from full; brw_batch's schema says they are the same")
	}
}
