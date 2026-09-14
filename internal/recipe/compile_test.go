package recipe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

const (
	compileOrigin   = "https://reports.example.test"
	compileSignedIn = "Signed in as fixture operator"
)

func element(ref, role, name string) snapshot.Element {
	return snapshot.Element{Ref: ref, Role: role, Name: name, Visible: true, InViewport: true, NameIsVisibleText: true}
}

func field(ref, name string) snapshot.Element {
	return snapshot.Element{Ref: ref, Role: "textbox", Name: name, Tag: "input", Visible: true, InViewport: true}
}

// sixStepLoggedInTrace is one recorded pass through a signed-in reporting site:
// open it, search, open a report, name an export, request it, confirm. Every
// page carries the signed-in banner, which is what makes it a logged-in flow
// rather than an anonymous one: none of these pages exists for a signed-out
// session.
func sixStepLoggedInTrace() []TraceStep {
	list := TraceObservation{URL: compileOrigin + "/reports", Elements: []snapshot.Element{
		element("e1", "heading", compileSignedIn),
		element("e2", "button", "Search reports"),
	}}
	search := TraceObservation{URL: compileOrigin + "/reports", Elements: []snapshot.Element{
		element("e1", "heading", compileSignedIn),
		element("e2", "button", "Search reports"),
		field("e3", "Report name"),
	}}
	results := TraceObservation{URL: compileOrigin + "/reports", Elements: []snapshot.Element{
		element("e1", "heading", compileSignedIn),
		element("e2", "button", "Search reports"),
		field("e3", "Report name"),
		element("e4", "link", "Quarterly revenue"),
	}}
	report := TraceObservation{URL: compileOrigin + "/reports/quarterly-revenue", Elements: []snapshot.Element{
		element("e1", "heading", compileSignedIn),
		element("e5", "heading", "Quarterly revenue"),
		field("e6", "Export file name"),
		element("e7", "button", "Request export"),
	}}
	requested := TraceObservation{URL: compileOrigin + "/reports/quarterly-revenue", Elements: []snapshot.Element{
		element("e1", "heading", compileSignedIn),
		element("e5", "heading", "Quarterly revenue"),
		field("e6", "Export file name"),
		element("e7", "button", "Request export"),
		element("e8", "status", "Export queued"),
	}}
	done := TraceObservation{URL: compileOrigin + "/reports/quarterly-revenue/exports", Elements: []snapshot.Element{
		element("e1", "heading", compileSignedIn),
		element("e9", "heading", "Exports"),
		element("e10", "status", "Export ready"),
	}}
	return []TraceStep{
		{TraceAction: TraceAction{Action: "navigate_to", URL: compileOrigin + "/reports", OK: true}, After: &list},
		{TraceAction: TraceAction{Action: "click", Ref: "e2", Role: "button", Name: "Search reports", NameIsVisibleText: true, OK: true}, Before: &list, After: &search},
		{TraceAction: TraceAction{Action: "fill", Ref: "e3", Role: "textbox", Name: "Report name", Text: "fixture typed search phrase", OK: true}, Before: &search, After: &results},
		{TraceAction: TraceAction{Action: "click", Ref: "e4", Role: "link", Name: "Quarterly revenue", NameIsVisibleText: true, OK: true}, Before: &results, After: &report},
		{TraceAction: TraceAction{Action: "fill", Ref: "e6", Role: "textbox", Name: "Export file name", Text: "fixture-export-name.csv", OK: true}, Before: &report, After: &requested},
		{TraceAction: TraceAction{Action: "click", Ref: "e7", Role: "button", Name: "Request export", NameIsVisibleText: true, OK: true}, Before: &requested, After: &done},
	}
}

func compileOptions() CompileOptions {
	return CompileOptions{
		ID:          "example.reports.export-quarterly",
		Version:     "1.0.0",
		Name:        "Export the quarterly revenue report",
		Description: "Search the signed-in reporting site for a report and request an export of it.",
		Intents:     []string{"export a revenue report"},
		Origins:     []string{compileOrigin},
		Risk:        "read_only",
	}
}

func mustCompile(t *testing.T, steps []TraceStep, opts CompileOptions) CompileResult {
	t.Helper()
	result, err := Compile(steps, opts)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return result
}

// replaySurface replays the recorded page sequence: every semantic target the
// compiled recipe resolves is resolved against the page state the runner has
// reached, and every postcondition is evaluated against the page the action
// left behind. A target the compiler emitted that matches nothing, or two
// things, fails here exactly as it would in a browser.
type replaySurface struct {
	pages  []TraceObservation
	cursor int
	acted  int
}

func newReplaySurface(steps []TraceStep) *replaySurface {
	pages := []TraceObservation{}
	for index, step := range steps {
		if index == 0 {
			before := step.Before
			if before == nil {
				before = &TraceObservation{URL: compileOrigin + "/"}
			}
			pages = append(pages, *before)
		}
		pages = append(pages, *step.After)
	}
	return &replaySurface{pages: pages}
}

func (s *replaySurface) page() TraceObservation { return s.pages[s.cursor] }

func (s *replaySurface) advance() error {
	s.acted++
	if s.cursor+1 < len(s.pages) {
		s.cursor++
	}
	return nil
}

func (s *replaySurface) Origin(context.Context) (string, error) {
	origin := originOf(s.page().URL)
	if origin == "" {
		return "", errors.New("replayed page has no origin")
	}
	return origin, nil
}

func (s *replaySurface) candidates(target Target) []snapshot.Element {
	return snapshot.RankTargetCandidates(s.page().Elements, targetCriteria(target))
}

func (s *replaySurface) Resolve(_ context.Context, target Target) ([]ResolvedElement, error) {
	out := []ResolvedElement{}
	for _, candidate := range s.candidates(target) {
		out = append(out, ResolvedElement{Ref: candidate.Ref, Role: candidate.Role, Name: candidate.Name})
	}
	return out, nil
}

func (s *replaySurface) Click(context.Context, string) error          { return s.advance() }
func (s *replaySurface) Fill(context.Context, string, string) error   { return s.advance() }
func (s *replaySurface) Type(context.Context, string, string) error   { return s.advance() }
func (s *replaySurface) Select(context.Context, string, string) error { return s.advance() }
func (s *replaySurface) Press(context.Context, string, string) error  { return s.advance() }
func (s *replaySurface) NavigateTo(context.Context, string) error     { return s.advance() }
func (s *replaySurface) Capture(context.Context, CaptureSpec) (artifact.Meta, error) {
	return artifact.Meta{}, errors.New("replay surface captures nothing")
}

func (s *replaySurface) EventSatisfied(_ context.Context, event Event) (bool, error) {
	page := s.page()
	switch event.Kind {
	case "url.matches":
		return strings.Contains(page.URL, event.Match), nil
	case "element.visible":
		return len(s.candidates(*event.Target)) == 1, nil
	case "download.completed":
		for _, download := range page.Downloads {
			if download.Completed && strings.Contains(download.Filename, event.Match) {
				return true, nil
			}
		}
		return false, nil
	case "text.present":
		for _, candidate := range page.Elements {
			if strings.Contains(candidate.Name, event.Match) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("replay surface cannot evaluate %q", event.Kind)
}

func (s *replaySurface) WaitEvent(ctx context.Context, event Event) error {
	satisfied, err := s.EventSatisfied(ctx, event)
	if err != nil {
		return err
	}
	if !satisfied {
		return fmt.Errorf("postcondition %s %q did not hold on %s", event.Kind, event.Match, s.page().URL)
	}
	return nil
}

func (s *replaySurface) Assert(ctx context.Context, assertion Assertion) error {
	page := s.page()
	switch assertion.Kind {
	case browser.AssertionURL:
		if page.URL != assertion.Expected {
			return fmt.Errorf("url assertion wanted %q, page is %q", assertion.Expected, page.URL)
		}
		return nil
	case browser.AssertionElementCount:
		count := len(s.candidates(*assertion.Target))
		if assertion.Count != nil && count != *assertion.Count {
			return fmt.Errorf("element_count wanted %d, got %d", *assertion.Count, count)
		}
		if assertion.Min != nil && count < *assertion.Min {
			return fmt.Errorf("element_count wanted at least %d, got %d", *assertion.Min, count)
		}
		return nil
	case browser.AssertionDownload:
		for _, download := range page.Downloads {
			if download.Filename == assertion.Filename {
				return nil
			}
		}
		return fmt.Errorf("download %q is not present", assertion.Filename)
	}
	return fmt.Errorf("replay surface cannot assert %q", assertion.Kind)
}

// TestCompiledSixStepFlowReplaysGreenTwice drives the compiled recipe through a
// runner twice, each time against a surface that starts from the first recorded
// page — the equivalent of a fresh profile for everything a recipe can observe.
// Both runs must complete, and neither may resolve a target ambiguously or miss
// an inferred postcondition.
func TestCompiledSixStepFlowReplaysGreenTwice(t *testing.T) {
	trace := sixStepLoggedInTrace()
	result := mustCompile(t, trace, compileOptions())
	inputs := map[string]string{
		"report_name":      "fixture typed search phrase",
		"export_file_name": "fixture-export-name.csv",
	}
	for run := 1; run <= 2; run++ {
		surface := newReplaySurface(trace)
		outcome, err := (Runner{Surface: surface}).Run(context.Background(), result.Recipe, inputs)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if outcome.Status != "done" {
			t.Fatalf("run %d status = %q", run, outcome.Status)
		}
		if surface.acted != 6 {
			t.Fatalf("run %d performed %d browser actions, want the six recorded ones", run, surface.acted)
		}
	}
}

func TestCompileDerivesSemanticTargetsAndNeverEmitsRefs(t *testing.T) {
	result := mustCompile(t, sixStepLoggedInTrace(), compileOptions())
	body, err := json.Marshal(result.Recipe)
	if err != nil {
		t.Fatal(err)
	}
	if refPattern.Match(body) {
		t.Fatalf("compiled recipe carried an observation ref:\n%s", body)
	}
	for _, forbidden := range []string{"data-brw-ref", "querySelector", "\"cx\"", "\"cy\""} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("compiled recipe carried %q:\n%s", forbidden, body)
		}
	}
	wanted := map[string]Target{
		"s2": {Role: "button", Name: "Search reports"},
		"s3": {Role: "textbox", NameContains: "Report name"},
		"s4": {Role: "link", Name: "Quarterly revenue"},
		"s6": {Role: "button", Name: "Request export"},
	}
	for _, step := range result.Recipe.Steps {
		want, ok := wanted[step.ID]
		if !ok {
			continue
		}
		if step.Target == nil || *step.Target != want {
			t.Fatalf("step %s target = %+v, want %+v", step.ID, step.Target, want)
		}
	}
	if len(result.Targets) != 5 {
		t.Fatalf("recorded %d derived targets, want one per targeted step", len(result.Targets))
	}
	for _, derived := range result.Targets {
		if derived.Ordinal != 1 || derived.Candidates != 1 {
			t.Fatalf("derived target %+v: a compiled target must be the sole candidate", derived)
		}
		if derived.Origin != compileOrigin {
			t.Fatalf("derived target %+v carries no recorded origin", derived)
		}
	}
}

func TestCompileDropsTypedTextAndDeclaresRuntimeInputs(t *testing.T) {
	result := mustCompile(t, sixStepLoggedInTrace(), compileOptions())
	body, err := json.Marshal(result.Recipe)
	if err != nil {
		t.Fatal(err)
	}
	for _, typed := range []string{"fixture typed search phrase", "fixture-export-name.csv"} {
		if strings.Contains(string(body), typed) {
			t.Fatalf("compiled recipe inlined the recorded value %q:\n%s", typed, body)
		}
	}
	for stepID, input := range map[string]string{"s3": "report_name", "s5": "export_file_name"} {
		step := stepByID(t, result.Recipe, stepID)
		if step.Value != "${input:"+input+"}" {
			t.Fatalf("step %s value = %q, want a placeholder for %s", stepID, step.Value, input)
		}
		declared, ok := result.Recipe.Inputs[input]
		if !ok || !declared.Required {
			t.Fatalf("input %q was not declared as required: %+v", input, result.Recipe.Inputs)
		}
	}
}

func TestCompileInfersPostconditionsFromTheObservation(t *testing.T) {
	before := TraceObservation{URL: compileOrigin + "/one", Elements: []snapshot.Element{element("e1", "button", "Go")}}
	navigated := TraceObservation{URL: compileOrigin + "/two", Elements: []snapshot.Element{element("e1", "button", "Go")}}
	revealed := TraceObservation{URL: compileOrigin + "/one", Elements: []snapshot.Element{
		element("e1", "button", "Go"), element("e2", "status", "Saved"),
	}}
	downloaded := TraceObservation{
		URL:       compileOrigin + "/one",
		Elements:  []snapshot.Element{element("e1", "button", "Go")},
		Downloads: []TraceDownload{{Filename: "statement.csv", Bytes: int64Pointer(2048), Completed: true}},
	}

	tests := []struct {
		name      string
		after     TraceObservation
		wantKind  string
		wantMatch string
		wantNext  string
	}{
		{"resulting url", navigated, "url.matches", compileOrigin + "/two", browser.AssertionURL},
		{"element the step introduced", revealed, "element.visible", "", browser.AssertionElementCount},
		{"download completion", downloaded, "download.completed", "statement.csv", browser.AssertionDownload},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			after := test.after
			steps := []TraceStep{{
				TraceAction: TraceAction{Action: "click", Ref: "e1", Role: "button", Name: "Go", NameIsVisibleText: true, OK: true},
				Before:      &before, After: &after,
			}}
			result := mustCompile(t, steps, compileOptions())
			step := result.Recipe.Steps[0]
			if step.Postcondition == nil || step.Postcondition.Kind != test.wantKind {
				t.Fatalf("postcondition = %+v, want kind %q", step.Postcondition, test.wantKind)
			}
			if test.wantMatch != "" && step.Postcondition.Match != test.wantMatch {
				t.Fatalf("postcondition match = %q, want %q", step.Postcondition.Match, test.wantMatch)
			}
			if test.wantKind == "element.visible" {
				if step.Postcondition.Target == nil || step.Postcondition.Target.Name != "Saved" {
					t.Fatalf("element postcondition target = %+v, want the element the step introduced", step.Postcondition.Target)
				}
			}
			if len(result.Recipe.Steps) != 2 || result.Recipe.Steps[1].Assert == nil ||
				result.Recipe.Steps[1].Assert.Kind != test.wantNext {
				t.Fatalf("steps = %+v, want a following %s assertion", result.Recipe.Steps, test.wantNext)
			}
			if test.wantKind == "element.visible" {
				assertion := result.Recipe.Steps[1].Assert
				if assertion.Target == nil || assertion.Target.Name != "Saved" ||
					assertion.Count == nil || *assertion.Count != 1 {
					t.Fatalf("element assertion = %+v, want exactly one of the introduced element", assertion)
				}
			}
		})
	}
}

// TestCompileRefusesWhatCannotBecomeARecipe is the closed list of things that
// stop compilation dead. Each case asserts the step index, because "something
// in your trace" is not a report anyone can act on.
func TestCompileRefusesWhatCannotBecomeARecipe(t *testing.T) {
	base := TraceObservation{URL: compileOrigin + "/one", Elements: []snapshot.Element{
		element("e1", "button", "Go"), field("e2", "Card number"), field("e3", "Notes"),
	}}
	after := TraceObservation{URL: compileOrigin + "/two", Elements: []snapshot.Element{element("e1", "button", "Go")}}
	twins := TraceObservation{URL: compileOrigin + "/one", Elements: []snapshot.Element{
		element("e1", "link", "Open"), element("e9", "link", "Open"),
	}}
	sensitive := TraceObservation{URL: compileOrigin + "/one", Elements: []snapshot.Element{
		{Ref: "e3", Role: "textbox", Name: "Recovery phrase", Tag: "input", Type: "password", Sensitive: true, Visible: true},
	}}

	tests := []struct {
		name      string
		steps     []TraceStep
		wantIndex int
		wantHint  string
	}{
		{
			name: "coordinate click",
			steps: []TraceStep{
				{TraceAction: TraceAction{Action: "click", Ref: "e1", Role: "button", Name: "Go", NameIsVisibleText: true, OK: true}, Before: &base, After: &after},
				{TraceAction: TraceAction{Action: "click_xy", OK: true}, Before: &after, After: &after},
			},
			wantIndex: 2, wantHint: "coordinate-driven",
		},
		{
			name: "credential field named in the recording",
			steps: []TraceStep{
				{TraceAction: TraceAction{Action: "fill", Ref: "e2", Role: "textbox", Name: "Card number", Text: "fixture-card-value-one", OK: true}, Before: &base, After: &after},
			},
			wantIndex: 1, wantHint: "credential field",
		},
		{
			name: "credential field brw redacted",
			steps: []TraceStep{
				{TraceAction: TraceAction{Action: "fill", Ref: "e3", Role: "textbox", Name: "Notes", Redacted: true, OK: true}, Before: &base, After: &after},
			},
			wantIndex: 1, wantHint: "credential field",
		},
		{
			name: "credential field the snapshot marked sensitive",
			steps: []TraceStep{
				{TraceAction: TraceAction{Action: "fill", Ref: "e3", Role: "textbox", Name: "Notes", Text: "fixture-note-value-one", OK: true}, Before: &sensitive, After: &after},
			},
			wantIndex: 1, wantHint: "marked sensitive",
		},
		{
			name: "ambiguous target",
			steps: []TraceStep{
				{TraceAction: TraceAction{Action: "click", Ref: "e1", Role: "link", Name: "Open", NameIsVisibleText: true, OK: true}, Before: &twins, After: &after},
			},
			wantIndex: 1, wantHint: "ambiguous target",
		},
		{
			name: "undeclared cross-origin navigation",
			steps: []TraceStep{
				{
					TraceAction: TraceAction{Action: "click", Ref: "e1", Role: "button", Name: "Go", NameIsVisibleText: true, OK: true},
					Before:      &base,
					After:       &TraceObservation{URL: "https://elsewhere.example.test/landed", Elements: []snapshot.Element{element("e1", "heading", "Elsewhere")}},
				},
			},
			wantIndex: 1, wantHint: "not in the declared origin list",
		},
		{
			name: "no observation of the page the action was aimed at",
			steps: []TraceStep{
				{TraceAction: TraceAction{Action: "click", Ref: "e1", Role: "button", Name: "Go", NameIsVisibleText: true, OK: true}, After: &after},
			},
			wantIndex: 1, wantHint: "nothing was observed of the page",
		},
		{
			name: "empty post-action observation",
			steps: []TraceStep{
				{TraceAction: TraceAction{Action: "click", Ref: "e1", Role: "button", Name: "Go", NameIsVisibleText: true, OK: true}, Before: &base, After: &TraceObservation{}},
			},
			wantIndex: 1, wantHint: "post-action observation is empty",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Compile(test.steps, compileOptions())
			if err == nil {
				t.Fatal("compilation succeeded; it had to fail")
			}
			var failure CompileError
			if !errors.As(err, &failure) {
				t.Fatalf("error %v is not a CompileError, so no step index reached the operator", err)
			}
			if failure.StepIndex != test.wantIndex {
				t.Fatalf("named step %d, want step %d: %v", failure.StepIndex, test.wantIndex, err)
			}
			if !strings.Contains(failure.Reason, test.wantHint) {
				t.Fatalf("reason %q does not explain %q", failure.Reason, test.wantHint)
			}
		})
	}
}

// A credential field must not merely be redacted out of the output: nothing may
// be produced at all, so there is never a moment where a draft exists.
func TestCompileProducesNothingAtAllForACredentialField(t *testing.T) {
	trace := sixStepLoggedInTrace()
	trace[2].Redacted = true
	result, err := Compile(trace, compileOptions())
	if err == nil {
		t.Fatal("compilation succeeded on a credential field")
	}
	if len(result.Recipe.Steps) != 0 || result.Review != "" || len(result.Targets) != 0 {
		t.Fatalf("a failed compile returned material: %+v", result)
	}
}

func TestCompileRequiresDeclarationsARecordingCannotSupply(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CompileOptions)
		want   string
	}{
		{"no origins", func(o *CompileOptions) { o.Origins = nil }, "explicit origin allowlist"},
		{"no id", func(o *CompileOptions) { o.ID = "" }, "dotted lowercase id"},
		{"no version", func(o *CompileOptions) { o.Version = "v1" }, "semantic version"},
		{"no intents", func(o *CompileOptions) { o.Intents = nil }, "at least one intent"},
		{"no risk", func(o *CompileOptions) { o.Risk = "" }, "risk read_only or external_write"},
		{"no description", func(o *CompileOptions) { o.Description = "" }, "name and a description"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := compileOptions()
			test.mutate(&options)
			_, err := Compile(sixStepLoggedInTrace(), options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestCompileDeclaredWriteCarriesIdempotencyAndVerification(t *testing.T) {
	trace := sixStepLoggedInTrace()
	options := compileOptions()
	options.Risk = "external_write"
	count := 1
	options.Writes = map[int]WriteDeclaration{6: {Verify: Assertion{
		Kind: browser.AssertionElementCount, Target: &Target{Role: "status", Name: "Export ready"}, Count: &count,
	}}}
	result := mustCompile(t, trace, options)

	write := stepByID(t, result.Recipe, "s6")
	if write.Effect != "external_write" || strings.TrimSpace(write.IdempotencyKey) == "" {
		t.Fatalf("declared write compiled as %+v", write)
	}
	if write.Postcondition == nil || write.Postcondition.Kind != "url.matches" {
		t.Fatalf("declared write postcondition = %+v, want durable state", write.Postcondition)
	}
	if err := RequireWriteVerification(result.Recipe); err != nil {
		t.Fatalf("compiled write lacks the read-back it must declare: %v", err)
	}

	options.Writes = map[int]WriteDeclaration{6: {}}
	if _, err := Compile(trace, options); err == nil {
		t.Fatal("a write with no declared verification compiled")
	}
}

// A declared write whose only evidence is a download cannot be compiled: a
// download cannot be re-checked on a later run, so it can never tell a rerun
// whether the first attempt landed.
func TestCompileRefusesATransientOnlyWrite(t *testing.T) {
	before := TraceObservation{URL: compileOrigin + "/one", Elements: []snapshot.Element{element("e1", "button", "Go")}}
	after := TraceObservation{
		URL:       compileOrigin + "/one",
		Elements:  []snapshot.Element{element("e1", "button", "Go")},
		Downloads: []TraceDownload{{Filename: "receipt.pdf", Bytes: int64Pointer(10), Completed: true}},
	}
	options := compileOptions()
	options.Risk = "external_write"
	minimum := 1
	options.Writes = map[int]WriteDeclaration{1: {Verify: Assertion{
		Kind: browser.AssertionElementCount, Target: &Target{Role: "button", Name: "Go"}, Min: &minimum,
	}}}
	_, err := Compile([]TraceStep{{
		TraceAction: TraceAction{Action: "click", Ref: "e1", Role: "button", Name: "Go", NameIsVisibleText: true, OK: true},
		Before:      &before, After: &after,
	}}, options)
	if err == nil || !strings.Contains(err.Error(), "cannot be re-checked") {
		t.Fatalf("err = %v, want a refusal naming the transient evidence", err)
	}
}

func TestReviewBodyIsStableAndDiffsInProportionToTheChange(t *testing.T) {
	first := mustCompile(t, sixStepLoggedInTrace(), compileOptions())
	again := mustCompile(t, sixStepLoggedInTrace(), compileOptions())
	if first.Review != again.Review {
		t.Fatalf("recompiling the same trace produced a different review body:\n%s\n---\n%s", first.Review, again.Review)
	}
	if !strings.Contains(first.Review, "derived candidate 1 of 1 on "+compileOrigin) {
		t.Fatalf("review body does not show how a target was derived:\n%s", first.Review)
	}

	changed := sixStepLoggedInTrace()
	renamed := *changed[5].Before
	renamed.Elements = append([]snapshot.Element(nil), renamed.Elements...)
	renamed.Elements[3] = element("e7", "button", "Request export now")
	changed[5].Before = &renamed
	changed[5].Name = "Request export now"
	changed[4].After = &renamed
	third := mustCompile(t, changed, compileOptions())

	differing := 0
	before, afterLines := strings.Split(first.Review, "\n"), strings.Split(third.Review, "\n")
	if len(before) != len(afterLines) {
		t.Fatalf("one renamed button changed the shape of the review body")
	}
	for index := range before {
		if before[index] != afterLines[index] {
			differing++
		}
	}
	if differing == 0 || differing > 4 {
		t.Fatalf("one renamed button changed %d review lines; a diff must track the change", differing)
	}
}

func stepByID(t *testing.T, value Recipe, id string) Step {
	t.Helper()
	for _, step := range value.Steps {
		if step.ID == id {
			return step
		}
	}
	t.Fatalf("recipe has no step %q: %+v", id, value.Steps)
	return Step{}
}

func int64Pointer(value int64) *int64 { return &value }

// A write declaration is the most consequential line in a plan. One naming a
// step the trace does not contain must not be dropped: the flow would compile
// as though it wrote nothing.
func TestCompileRefusesAWriteDeclarationForAStepTheTraceDoesNotHave(t *testing.T) {
	count := 1
	verify := Assertion{Kind: browser.AssertionElementCount, Target: &Target{Role: "status", Name: "Export ready"}, Count: &count}
	options := compileOptions()
	options.Risk = "external_write"
	options.Writes = map[int]WriteDeclaration{6: {Verify: verify}, 9: {Verify: verify}}
	_, err := Compile(sixStepLoggedInTrace(), options)
	if err == nil || !strings.Contains(err.Error(), "[9]") {
		t.Fatalf("err = %v, want the unapplied declaration named", err)
	}
}

// The compiled recipe is hashed into a digest callers pin. Nothing a caller
// still holds may reach into what that digest covers.
func TestCompiledWriteCopiesTheDeclaredNonceRatherThanAliasingIt(t *testing.T) {
	count := 1
	nonce := SiteIdempotency{Kind: SiteIdempotencyFormNonce, Target: &Target{Role: "textbox", TestID: "submission-token"}}
	options := compileOptions()
	options.Risk = "external_write"
	options.Writes = map[int]WriteDeclaration{6: {
		Verify: Assertion{Kind: browser.AssertionElementCount, Target: &Target{Role: "status", Name: "Export ready"}, Count: &count},
		Nonce:  &nonce,
	}}
	result := mustCompile(t, sixStepLoggedInTrace(), options)
	before, err := Digest(result.Recipe)
	if err != nil {
		t.Fatal(err)
	}

	nonce.Target.TestID = "some-other-field"
	nonce.Kind = "tampered"

	write := stepByID(t, result.Recipe, "s6")
	if write.SiteIdempotency == nil || write.SiteIdempotency.Kind != SiteIdempotencyFormNonce ||
		write.SiteIdempotency.Target.TestID != "submission-token" {
		t.Fatalf("the plan reached into the compiled recipe: %+v", write.SiteIdempotency)
	}
	after, err := Digest(result.Recipe)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("the pinned digest moved after the plan changed: %s then %s", before, after)
	}
}

// The credential check is driven by a table rather than by a list of actions
// written into the check, so the table has to cover every action that can be
// compiled. An action added to compilableActions without a classification
// reaches a page carrying whatever the recording typed, which is the failure
// that let a keystroke-by-keystroke code through.
func TestEveryCompilableActionIsClassifiedForCredentialChecking(t *testing.T) {
	for action := range compilableActions {
		if _, classified := literalValueActions[action]; !classified {
			t.Errorf("compilable action %q has no entry in literalValueActions, so the credential check walks past it", action)
		}
	}
	for action := range literalValueActions {
		if _, compilable := compilableActions[action]; !compilable {
			t.Errorf("literalValueActions classifies %q, which is not a compilable action", action)
		}
	}
	// The classification only means something if an unclassified action is
	// refused rather than waved through.
	before := TraceObservation{URL: compileOrigin + "/one", Elements: []snapshot.Element{element("e1", "button", "Go")}}
	compiler, err := newCompiler(compileOptions())
	if err != nil {
		t.Fatal(err)
	}
	err = compiler.checkCredentialField(1, TraceStep{
		TraceAction: TraceAction{Action: "an_action_nobody_classified", Ref: "e1"},
	}, &before)
	if err == nil || !strings.Contains(err.Error(), "no classification") {
		t.Fatalf("err = %v, want an unclassified action refused", err)
	}
}

// A code entered one keystroke at a time is the value, spelled differently:
// every character reaches step.Key, and the schema bounds Key only by length.
func TestCompileRefusesWhatAPressStepWouldCarry(t *testing.T) {
	base := TraceObservation{URL: compileOrigin + "/one", Elements: []snapshot.Element{
		{Ref: "e1", Role: "textbox", Name: "Notes", Tag: "input", Visible: true, InViewport: true},
		{Ref: "e2", Role: "textbox", Name: "One-time code", Tag: "input", Visible: true, InViewport: true},
	}}
	after := TraceObservation{URL: compileOrigin + "/two", Elements: []snapshot.Element{element("e3", "heading", "Done")}}

	tests := []struct {
		name     string
		traced   TraceAction
		wantHint string
	}{
		{
			name:     "a literal character press",
			traced:   TraceAction{Action: "press", Ref: "e1", Role: "textbox", Name: "Notes", Text: "7", OK: true},
			wantHint: "literal character rather than a named key",
		},
		{
			name:     "a press into a credential field",
			traced:   TraceAction{Action: "press", Ref: "e2", Role: "textbox", Name: "One-time code", Text: "Enter", OK: true},
			wantHint: "credential field",
		},
		{
			name:     "a press brw redacted",
			traced:   TraceAction{Action: "press", Ref: "e1", Role: "textbox", Name: "Notes", Text: "Enter", Redacted: true, OK: true},
			wantHint: "credential field",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Compile([]TraceStep{{TraceAction: test.traced, Before: &base, After: &after}}, compileOptions())
			if err == nil || !strings.Contains(err.Error(), test.wantHint) {
				t.Fatalf("err = %v, want it to mention %q", err, test.wantHint)
			}
			if err != nil && strings.Contains(err.Error(), "\"7\"") {
				t.Fatalf("the refusal quoted the recorded keystroke back: %v", err)
			}
		})
	}

	// A named key is what press is for, and still compiles.
	result := mustCompile(t, []TraceStep{{
		TraceAction: TraceAction{Action: "press", Ref: "e1", Role: "textbox", Name: "Notes", Text: "Enter", OK: true},
		Before:      &base, After: &after,
	}}, compileOptions())
	if step := result.Recipe.Steps[0]; step.Action != "press" || step.Key != "Enter" {
		t.Fatalf("a named key press compiled as %+v", step)
	}
}

// The compiler emits two assertions after a declared write, and which one reads
// remote state back is not something their order says. An interrupted rerun
// that consults the inferred evidence assertion commits a receipt for a write
// nothing confirmed, and a committed receipt suppresses the write for good.
func TestACompiledWriteTagsTheDeclaredReadBack(t *testing.T) {
	count := 1
	options := compileOptions()
	options.Risk = "external_write"
	options.Writes = map[int]WriteDeclaration{6: {Verify: Assertion{
		Kind: browser.AssertionElementCount, Target: &Target{Role: "status", Name: "Export ready"}, Count: &count,
	}}}
	result := mustCompile(t, sixStepLoggedInTrace(), options)

	write := stepByID(t, result.Recipe, "s6")
	asserts := []string{}
	for _, step := range result.Recipe.Steps {
		if step.Action == "assert" && strings.HasPrefix(step.ID, "s6") {
			asserts = append(asserts, step.ID)
		}
	}
	if len(asserts) != 2 {
		t.Fatalf("the write is followed by %v, want both the inferred evidence and the declared read-back", asserts)
	}
	verification, err := WriteVerification(result.Recipe, write)
	if err != nil {
		t.Fatal(err)
	}
	if verification.ID != "s6_verify" {
		t.Fatalf("the write is verified by %q, want the declared read-back s6_verify", verification.ID)
	}
	if verification.Assert.Kind != browser.AssertionElementCount {
		t.Fatalf("the verification is a %s assertion, want the declared one", verification.Assert.Kind)
	}
	if stepByID(t, result.Recipe, "s6_verify").Verifies != "s6" {
		t.Fatal("the declared read-back is not tagged with the write it verifies")
	}
}

// Two untagged assertions after a write are two candidate read-backs and no way
// to choose, so the recipe is refused rather than guessed at.
func TestUntaggedAmbiguousVerificationIsRefused(t *testing.T) {
	value := validRecipe(compileOrigin)
	value.Risk = "external_write"
	minimum := 1
	assertion := func(id string) Step {
		return Step{ID: id, Action: "assert", Assert: &Assertion{
			Kind: browser.AssertionElementCount, Target: &Target{Role: "status", Name: "Sent"}, Min: &minimum,
		}}
	}
	write := Step{
		ID: "send", Action: "click", Effect: "external_write",
		Target:         &Target{Role: "button", Name: "Send"},
		IdempotencyKey: "send",
		Postcondition:  &Event{Kind: "text.present", Match: "sent", TimeoutMS: 100},
	}
	value.Steps = []Step{write, assertion("evidence"), assertion("read_back")}
	if err := RequireWriteVerification(value); err == nil || !strings.Contains(err.Error(), "untagged assert steps") {
		t.Fatalf("err = %v, want the ambiguity refused", err)
	}

	tagged := value.Steps[2]
	tagged.Verifies = "send"
	value.Steps[2] = tagged
	if err := RequireWriteVerification(value); err != nil {
		t.Fatalf("a tagged read-back was refused: %v", err)
	}
	verification, err := WriteVerification(value, write)
	if err != nil || verification.ID != "read_back" {
		t.Fatalf("verification = %+v err = %v, want the tagged step", verification, err)
	}
}

// Service.acquireRunLocks singleflights on the expanded idempotency key, so a
// key that is the same string for every run makes two runs with unrelated
// inputs one transaction, and prints as though it named a submission.
func TestACompiledWriteKeyNamesTheInputsItDependsOn(t *testing.T) {
	count := 1
	options := compileOptions()
	options.Risk = "external_write"
	options.Writes = map[int]WriteDeclaration{6: {Verify: Assertion{
		Kind: browser.AssertionElementCount, Target: &Target{Role: "status", Name: "Export ready"}, Count: &count,
	}}}
	result := mustCompile(t, sixStepLoggedInTrace(), options)
	key := stepByID(t, result.Recipe, "s6").IdempotencyKey

	for name := range result.Recipe.Inputs {
		if !strings.Contains(key, "${input:"+name+"}") {
			t.Fatalf("idempotency key %q does not depend on declared input %q", key, name)
		}
	}
	first, err := Expand(key, map[string]string{"report_name": "one", "export_file_name": "a.csv"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Expand(key, map[string]string{"report_name": "two", "export_file_name": "b.csv"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two runs with different inputs expand to one key %q, so they serialise on each other", first)
	}
}

// A recorded href carries the recording's query string, and a query string is
// where a session id or a one-time token lives. Matching on it publishes what
// the recording held and stops replaying the moment the token expires.
func TestCompiledHrefTargetsDropTheRecordingsQueryString(t *testing.T) {
	const recordedQuery = "?session=fixture-token-value&page=2"
	before := TraceObservation{URL: compileOrigin + "/one", Elements: []snapshot.Element{
		{Ref: "e1", Role: "link", Name: "Open", Href: "/reports/quarterly" + recordedQuery, Visible: true, InViewport: true},
		{Ref: "e2", Role: "link", Name: "Open", Href: "/reports/annual" + recordedQuery, Visible: true, InViewport: true},
	}}
	after := TraceObservation{URL: compileOrigin + "/two", Elements: []snapshot.Element{element("e3", "heading", "Quarterly")}}
	result := mustCompile(t, []TraceStep{{
		TraceAction: TraceAction{Action: "click", Ref: "e1", Role: "link", OK: true},
		Before:      &before, After: &after,
	}}, compileOptions())

	target := result.Recipe.Steps[0].Target
	if target == nil || target.HrefContains != "/reports/quarterly" {
		t.Fatalf("target = %+v, want the href without the recording's query", target)
	}
	body, err := json.Marshal(result.Recipe)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "fixture-token-value") {
		t.Fatalf("the recording's one-time token reached the draft:\n%s", body)
	}
}

// The review body is what a human approves before --publish sends the draft to
// the provider. A navigation URL's query string is the other place a recording
// hides a one-time token, and at the end of a long URL a reviewer reads past it.
func TestReviewBodyShowsWhatARecordingCanHide(t *testing.T) {
	const recorded = compileOrigin + "/reports?session=fixture-session-id#access=fixture-grant-value"
	navigated := TraceObservation{URL: recorded, Elements: []snapshot.Element{element("e1", "button", "Go")}}
	unchanged := TraceObservation{URL: recorded, Elements: []snapshot.Element{element("e1", "button", "Go")}}
	result := mustCompile(t, []TraceStep{
		{TraceAction: TraceAction{Action: "navigate_to", URL: recorded, OK: true}, After: &navigated},
		{TraceAction: TraceAction{Action: "click", Ref: "e1", Role: "button", Name: "Go", NameIsVisibleText: true, OK: true}, Before: &navigated, After: &unchanged},
	}, compileOptions())

	// A session id in the query and an implicit-flow grant in the fragment are
	// the two places a recorded navigation hides one.
	for _, want := range []string{"url_query session=fixture-session-id", "url_fragment access=fixture-grant-value"} {
		if !strings.Contains(result.Review, want) {
			t.Fatalf("the review body does not isolate %q:\n%s", want, result.Review)
		}
	}
	// The click changed nothing observable, so the compiler inferred no
	// postcondition for it. Silence reads as a rendering that omits the line.
	if !strings.Contains(result.Review, "postcondition none") {
		t.Fatalf("the review body does not report the step with no postcondition:\n%s", result.Review)
	}
}

// ReviewBody is exported and documented as a rendering. A recipe that has not
// been through Validate is exactly the recipe somebody renders to find out what
// is wrong with it.
func TestReviewBodyRendersARecipeThatWouldNotValidate(t *testing.T) {
	value := Recipe{
		SchemaVersion: SchemaVersion, ID: "example.a.b", Version: "1.0.0",
		Name: "n", Description: "d", Intents: []string{"i"}, Origins: []string{compileOrigin},
		Risk: "external_write",
		Steps: []Step{{
			ID: "send", Action: "click", Effect: "external_write",
			SiteIdempotency: &SiteIdempotency{Kind: SiteIdempotencyFormNonce},
		}},
	}
	body := ReviewBody(value, nil)
	if !strings.Contains(body, "site_idempotency "+SiteIdempotencyFormNonce) {
		t.Fatalf("the rendering dropped the declared mechanism:\n%s", body)
	}
}

// A target that is unique in the recording can be ambiguous on the page a rerun
// meets. The replay must fail then, rather than pick one.
func TestCompiledReplayFailsWhenAReplayedPageIsAmbiguous(t *testing.T) {
	trace := sixStepLoggedInTrace()
	result := mustCompile(t, trace, compileOptions())
	inputs := map[string]string{
		"report_name":      "fixture typed search phrase",
		"export_file_name": "fixture-export-name.csv",
	}
	// The page a rerun meets has grown a second element with the same identity
	// as the one the recording clicked. Observations are shared between the
	// steps either side of an action, so each one is twinned exactly once.
	twinned := sixStepLoggedInTrace()
	seen := map[*TraceObservation]bool{}
	for index := range twinned {
		for _, observation := range []*TraceObservation{twinned[index].Before, twinned[index].After} {
			if observation == nil || seen[observation] {
				continue
			}
			seen[observation] = true
			for _, candidate := range observation.Elements {
				if candidate.Name == "Search reports" {
					observation.Elements = append(observation.Elements,
						element("e99", "button", "Search reports"))
					break
				}
			}
		}
	}
	_, err := (Runner{Surface: newReplaySurface(twinned)}).Run(context.Background(), result.Recipe, inputs)
	if err == nil || !strings.Contains(err.Error(), "resolved to 2 elements") {
		t.Fatalf("err = %v, want the replay to refuse an ambiguous target", err)
	}
}
