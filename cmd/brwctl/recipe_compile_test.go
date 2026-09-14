package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/snapshot"
)

const (
	compileTestOrigin = "https://reports.example.test"
	compileTestList   = compileTestOrigin + "/reports"
	compileTestReport = compileTestOrigin + "/reports/quarterly"
)

func visibleElement(ref, role, name string) snapshot.Element {
	return snapshot.Element{Ref: ref, Role: role, Name: name, Visible: true, InViewport: true, NameIsVisibleText: true}
}

// compileTrace is the smallest scoped trace that exercises the whole path: a
// navigation, a typed value that must become a runtime input, and a click that
// moved the page somewhere the compiler can infer a postcondition from.
func compileTrace(t *testing.T, credential bool) string {
	t.Helper()
	list := recipe.TraceObservation{URL: compileTestList, Elements: []snapshot.Element{
		visibleElement("e1", "heading", "Reports"),
		{Ref: "e2", Role: "textbox", Name: "Report name", Tag: "input", Visible: true, InViewport: true},
		visibleElement("e3", "button", "Open report"),
	}}
	filled := recipe.TraceObservation{URL: compileTestList, Elements: append([]snapshot.Element(nil),
		append(list.Elements, visibleElement("e4", "status", "One match"))...)}
	opened := recipe.TraceObservation{URL: compileTestReport, Elements: []snapshot.Element{
		visibleElement("e5", "heading", "Quarterly report"),
	}}
	fieldName := "Report name"
	if credential {
		fieldName = "Account password"
		list.Elements[1].Name = fieldName
		filled.Elements[1].Name = fieldName
	}
	steps := []recipe.TraceStep{
		{TraceAction: recipe.TraceAction{Action: "navigate_to", URL: compileTestList, OK: true}, After: &list},
		{
			TraceAction: recipe.TraceAction{Action: "fill", Ref: "e2", Role: "textbox", Name: fieldName, Text: "fixture typed value", OK: true},
			Before:      &list, After: &filled,
		},
		{
			TraceAction: recipe.TraceAction{Action: "click", Ref: "e3", Role: "button", Name: "Open report", NameIsVisibleText: true, OK: true},
			Before:      &filled, After: &opened,
		},
	}
	return writeFixture(t, "trace.json", steps)
}

func compilePlan(t *testing.T) string {
	t.Helper()
	return writeFixture(t, "plan.json", recipe.CompileOptions{
		ID:          "example.reports.open-quarterly",
		Version:     "1.0.0",
		Name:        "Open the quarterly report",
		Description: "Search the reporting site and open the named report.",
		Intents:     []string{"open a named report"},
		Origins:     []string{compileTestOrigin},
		Risk:        "read_only",
	})
}

func writeFixture(t *testing.T, name string, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecipeCompileWritesAnImmutableReadyDraft(t *testing.T) {
	out := filepath.Join(t.TempDir(), "draft.json")
	if err := recipeDraft([]string{
		"--from-trace", compileTrace(t, false), "--plan", compilePlan(t), "--out", out,
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("draft permissions %o are too broad", info.Mode().Perm())
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// Parse is the strict reader the install path uses. A compiled draft that
	// does not survive it would need hand-editing, which is what compiling is
	// meant to remove.
	value, err := recipe.Parse(body)
	if err != nil {
		t.Fatalf("compiled draft does not parse: %v\n%s", err, body)
	}
	if strings.Contains(string(body), "fixture typed value") {
		t.Fatalf("compiled draft inlined the recorded value:\n%s", body)
	}
	if strings.Contains(string(body), recipe.TodoMarker) {
		t.Fatalf("compiled draft left a marker for a human to resolve:\n%s", body)
	}
	if len(value.Inputs) != 1 {
		t.Fatalf("compiled draft declared %d inputs, want the typed field", len(value.Inputs))
	}
}

// A credential field stops compilation, and nothing is written. Writing a draft
// and cleaning it up afterwards would still have put the flow on disk.
func TestRecipeCompileWritesNothingWhenTheTraceCarriesACredentialField(t *testing.T) {
	out := filepath.Join(t.TempDir(), "draft.json")
	err := recipeDraft([]string{
		"--from-trace", compileTrace(t, true), "--plan", compilePlan(t), "--out", out,
	})
	if err == nil || !strings.Contains(err.Error(), "credential field") {
		t.Fatalf("err = %v, want a refusal naming the credential field", err)
	}
	if _, statErr := os.Lstat(out); !os.IsNotExist(statErr) {
		t.Fatalf("a refused compile left %s behind (stat err = %v)", out, statErr)
	}
	if entries, readErr := os.ReadDir(filepath.Dir(out)); readErr != nil || len(entries) != 0 {
		t.Fatalf("a refused compile left %v behind (err = %v)", entries, readErr)
	}
}

func TestRecipeCompileRefusesADestinationInsideAGitCheckout(t *testing.T) {
	checkout := t.TempDir()
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(checkout, "internal", "recipe")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(nested, "draft.json")
	err := recipeDraft([]string{
		"--from-trace", compileTrace(t, false), "--plan", compilePlan(t), "--out", out,
	})
	if err == nil || !strings.Contains(err.Error(), "outside every git checkout") {
		t.Fatalf("err = %v, want the checkout destination refused", err)
	}
	if _, statErr := os.Lstat(out); !os.IsNotExist(statErr) {
		t.Fatalf("a refused destination was written anyway")
	}
}

func TestRecipeCompileRejectsAnUnknownPlanField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(`{"id":"example.a.b","version":"1.0.0","writs":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := recipeDraft([]string{"--from-trace", compileTrace(t, false), "--plan", path, "--out", ""})
	if err == nil || !strings.Contains(err.Error(), "compile plan") {
		t.Fatalf("err = %v, want a misspelled plan key rejected", err)
	}
}

func TestRecipePublishNeedsACompilePlanAndAProviderURL(t *testing.T) {
	t.Setenv("BRW_RECIPE_PROVIDER_URL", "")
	if err := recipeDraft([]string{
		"--from-trace", compileTrace(t, false), "--out", filepath.Join(t.TempDir(), "d.json"),
		"--id", "example.a.b", "--publish",
	}); err == nil || !strings.Contains(err.Error(), "--plan") {
		t.Fatalf("err = %v, want publishing a skeleton refused", err)
	}
	if err := recipeDraft([]string{
		"--from-trace", compileTrace(t, false), "--plan", compilePlan(t), "--publish",
	}); err == nil || !strings.Contains(err.Error(), "BRW_RECIPE_PROVIDER_URL") {
		t.Fatalf("err = %v, want the missing provider write API named", err)
	}
}
