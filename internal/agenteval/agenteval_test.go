package agenteval

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/agenteval/solver"
	"github.com/Don-Works/brw/internal/harness"
	"github.com/Don-Works/brw/internal/snapshot"
)

// TestEveryTaskIsWellFormed walks the task table itself, so a task added
// without a fixture, a goal or a grader fails here instead of minutes into a
// run.
func TestEveryTaskIsWellFormed(t *testing.T) {
	tasks := Tasks()
	if len(tasks) != 4 {
		t.Fatalf("the evaluation set holds %d tasks, want the four it is specified as", len(tasks))
	}
	seen := map[string]bool{}
	for _, task := range tasks {
		t.Run(task.ID, func(t *testing.T) {
			if task.ID == "" {
				t.Fatal("task has no id")
			}
			if seen[task.ID] {
				t.Fatalf("duplicate task id %q", task.ID)
			}
			seen[task.ID] = true
			if task.Title == "" {
				t.Error("task has no title")
			}
			if strings.TrimSpace(task.Goal) == "" {
				t.Error("task states no goal, so nothing was asked of the agent")
			}
			if len(task.EndStateCriteria) == 0 {
				t.Error("task lists no end-state criteria, so the optional judge would be shown nothing to grade against")
			}
			if task.Solve == nil || task.Probe == nil || task.Check == nil {
				t.Fatal("task is missing a solver, a probe or a grader")
			}
			path := filepath.Join("..", "..", "tests", "fixtures", task.Fixture)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("task names fixture %s: %v", task.Fixture, err)
			}
			if found, ok := TaskByID(task.ID); !ok || found.ID != task.ID {
				t.Fatalf("TaskByID cannot find %q", task.ID)
			}
		})
	}
	if _, ok := TaskByID("no-such-task"); ok {
		t.Error("TaskByID found a task that does not exist")
	}
}

// TestEvaluationFixturesReachNothingOffThisMachine backs the claim the harness
// makes about itself. A task whose fixture fetched anything from another host
// would grade differently depending on that host's availability, and would do
// so silently.
func TestEvaluationFixturesReachNothingOffThisMachine(t *testing.T) {
	var paths []string
	seen := map[string]bool{}
	for _, task := range Tasks() {
		if seen[task.Fixture] {
			continue
		}
		seen[task.Fixture] = true
		paths = append(paths, filepath.Join("..", "..", "tests", "fixtures", task.Fixture))
	}
	if err := harness.AuditFixtures(paths); err != nil {
		t.Fatal(err)
	}
}

// TestNoTaskPassesWithoutEvidence is the property that makes the whole set an
// evaluation: given an end state the harness never observed, no task may report
// success — whatever the run claimed about itself.
//
// It enumerates the table rather than naming tasks, so a task added later
// cannot quietly be the one that passes on nothing.
func TestNoTaskPassesWithoutEvidence(t *testing.T) {
	claims := []struct {
		name    string
		outcome Outcome
	}{
		{name: "claiming success", outcome: Outcome{ClaimedSuccess: true, Answer: "64.99"}},
		{name: "claiming nothing", outcome: Outcome{}},
		{name: "reporting a failure", outcome: Outcome{FailureReport: "delete account control missing"}},
	}
	for _, task := range Tasks() {
		for _, claim := range claims {
			t.Run(task.ID+"/"+claim.name, func(t *testing.T) {
				verdict := task.Check(EndState{}, claim.outcome)
				if verdict.Passed {
					t.Fatalf("task %s passed on an end state the harness never observed", task.ID)
				}
				if len(verdict.Reasons) == 0 {
					t.Fatalf("task %s failed without naming a reason", task.ID)
				}
				if claim.outcome.ClaimedSuccess && !verdict.ClaimedWithoutReaching {
					t.Fatalf("task %s did not record that success was claimed without reaching the end state", task.ID)
				}
			})
		}
	}
}

// TestSabotagedRunIsGradedAFailure drives one task through the real harness —
// real headless Chrome, the real fixture origin, the real probe — in both
// modes.
//
// This is the assertion the evaluation rests on: an evaluation that cannot
// report a failure measures nothing. It runs ONE task, not the suite; the suite
// run is a measurement and lives behind `task eval`, not behind `go test`.
func TestSabotagedRunIsGradedAFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real browser")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	report, err := Run(ctx, Options{
		RepoRoot: filepath.Join("..", ".."),
		Only:     "form-submit",
		Modes:    Modes(),
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("got %d results, want one per mode", len(report.Results))
	}

	byMode := map[Mode]Result{}
	for _, result := range report.Results {
		byMode[result.Mode] = result
	}

	honest, ok := byMode[ModeHonest]
	if !ok {
		t.Fatal("no honest run in the report")
	}
	if !honest.Passed {
		t.Fatalf("the honest run failed, so the sabotage check below proves nothing: %v (error %q)", honest.Reasons, honest.Error)
	}

	sabotaged, ok := byMode[ModeSabotaged]
	if !ok {
		t.Fatal("no sabotaged run in the report")
	}
	if sabotaged.Passed {
		t.Fatal("the sabotaged run was graded a pass; this evaluation cannot fail and therefore measures nothing")
	}
	if !sabotaged.ClaimedSuccess {
		t.Fatal("the sabotaged run did not claim success, so it is not the case the grader has to catch")
	}
	if !sabotaged.ClaimedWithoutReaching {
		t.Error("the sabotaged run was not recorded as claiming success without reaching the end state")
	}
	if len(sabotaged.Reasons) == 0 {
		t.Error("the sabotaged run failed without naming what was missing")
	}
	// The end state has to come from the page, not from the run's own account of
	// itself: a sabotaged run that produced no observation would fail for the
	// wrong reason.
	if sabotaged.EndState.Fields == nil {
		t.Fatal("the sabotaged run recorded no observed end state")
	}
	if got := sabotaged.EndState.Field("terms"); got != "checked" {
		t.Errorf("observed terms = %q, want checked: the sabotage is meant to do everything except submit", got)
	}
	if got := sabotaged.EndState.Field("result"); got != "" {
		t.Errorf("observed status region = %q, want empty on a run that never submitted", got)
	}

	if report.OK != true {
		t.Errorf("report.OK = false; both runs graded the way their mode requires, so it should be true")
	}
	if report.Judge != "deterministic end-state check only" {
		t.Errorf("report says it was graded by %q; a run with no judge configured must say so", report.Judge)
	}
}

// waitPrefixPattern reads brw's own in-page wait grammar rather than a list
// written next to it, so a condition prefix added there shows up here.
var waitPrefixPattern = regexp.MustCompile(`condition\.indexOf\('([a-z_]+:)'\)===0`)

// nonScriptWaitPrefixes are the wait conditions that compare a value the caller
// supplies against page state. Each is a string comparison or a selector match,
// so none of them is a way to execute caller-supplied code.
var nonScriptWaitPrefixes = map[string]bool{
	"url:": true, "not_url:": true,
	"title:": true, "not_title:": true,
	"text:": true, "not_text:": true,
	"ref:": true, "not_ref:": true,
	"selector:": true, "not_selector:": true,
}

// TestEveryWaitConditionPrefixIsClassified is the guard on the solver surface.
//
// The Agent deliberately cannot run script in the page, and the wait grammar is
// the one place a caller-supplied string reaches an evaluator. A prefix added
// to that grammar and left unclassified here fails, rather than quietly
// becoming a way back in.
func TestEveryWaitConditionPrefixIsClassified(t *testing.T) {
	matches := waitPrefixPattern.FindAllStringSubmatch(snapshot.WaitConditionScript, -1)
	if len(matches) == 0 {
		t.Fatal("no wait condition prefixes found in the in-page script; this test would pass on anything")
	}
	scriptCarrying := map[string]bool{}
	for _, prefix := range solver.ScriptCarryingWaitPrefixes {
		scriptCarrying[prefix] = true
	}

	seen := map[string]bool{}
	for _, match := range matches {
		prefix := match[1]
		if seen[prefix] {
			continue
		}
		seen[prefix] = true
		if scriptCarrying[prefix] && nonScriptWaitPrefixes[prefix] {
			t.Errorf("wait prefix %q is classified both ways", prefix)
			continue
		}
		if !scriptCarrying[prefix] && !nonScriptWaitPrefixes[prefix] {
			t.Errorf("wait prefix %q is in neither list; decide whether it lets a solver run script in the page", prefix)
		}
	}
	for _, prefix := range solver.ScriptCarryingWaitPrefixes {
		if !seen[prefix] {
			t.Errorf("%q is refused but brw's wait grammar no longer has it", prefix)
		}
	}
}

// TestAgentRefusesAWaitThatCarriesScript proves the classification is acted on,
// not merely recorded.
func TestAgentRefusesAWaitThatCarriesScript(t *testing.T) {
	// The Agent carries no manager: the refusal has to happen before anything
	// reaches the browser, so a condition that got past it cannot quietly
	// succeed. The recover turns that into this test's failure rather than the
	// whole binary's.
	agent := solver.New(context.Background(), nil, "")
	refused := []string{
		"fn:document.title='x'",
		"FN:document.title='x'",
		"  fn:while(true){}",
	}
	for _, condition := range refused {
		t.Run(condition, func(t *testing.T) {
			if _, blocked := solver.IsScriptCarryingCondition(condition); !blocked {
				t.Fatalf("%q was not classified as script-carrying", condition)
			}
			err := waitWithoutCrashing(agent, condition)
			if err == nil {
				t.Fatalf("a solver was allowed to wait on %q", condition)
			}
			if !strings.Contains(err.Error(), "script") {
				t.Fatalf("error %q does not say why the condition was refused", err)
			}
		})
	}

	for _, condition := range []string{"text:Submitted", "ref:e12", "selector:#result", "load"} {
		if prefix, blocked := solver.IsScriptCarryingCondition(condition); blocked {
			t.Errorf("ordinary condition %q was classified as script-carrying via %q", condition, prefix)
		}
	}
}

// waitWithoutCrashing calls WaitFor on a manager-less Agent and turns the
// nil-pointer panic a removed guard would cause into an ordinary error.
func waitWithoutCrashing(agent *solver.Agent, condition string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = nil
		}
	}()
	return agent.WaitFor(condition, time.Second)
}

func TestSelectTasks(t *testing.T) {
	all, err := selectTasks("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(Tasks()) {
		t.Fatalf("empty filter selected %d tasks, want all %d", len(all), len(Tasks()))
	}
	one, err := selectTasks("basket-flow")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].ID != "basket-flow" {
		t.Fatalf("filter selected %d tasks", len(one))
	}
	if _, err := selectTasks("nope"); err == nil {
		t.Fatal("an unknown task id was accepted")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error %q does not name the unknown task", err)
	}
}

func TestModesCoversBothBehaviours(t *testing.T) {
	modes := Modes()
	if len(modes) != 2 {
		t.Fatalf("Modes() lists %d modes, want honest and sabotaged", len(modes))
	}
	found := map[Mode]bool{}
	for _, mode := range modes {
		found[mode] = true
	}
	if !found[ModeHonest] || !found[ModeSabotaged] {
		t.Fatalf("Modes() = %v, want both %q and %q", modes, ModeHonest, ModeSabotaged)
	}
}

func TestPriceNearReadsTheWholeAmount(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		product string
		want    string
	}{
		{
			name:    "price follows the product",
			text:    "2 result(s) Kiprun KS500 Running Shoes £64.99 View product",
			product: "Kiprun KS500 Running Shoes",
			want:    "£64.99",
		},
		{
			name:    "whole number price",
			text:    "Kalenji Run Support 100 £32 View product",
			product: "Kalenji Run Support 100",
			want:    "£32",
		},
		{
			name:    "product absent",
			text:    "nothing here",
			product: "Kiprun KS500",
			want:    "",
		},
		{
			name:    "no price after the product",
			text:    "Kiprun KS500 Running Shoes out of stock",
			product: "Kiprun KS500 Running Shoes",
			want:    "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := priceNear(testCase.text, testCase.product); got != testCase.want {
				t.Fatalf("priceNear = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestEndStateFieldTreatsAbsenceAsEmpty(t *testing.T) {
	var end EndState
	if got := end.Field("anything"); got != "" {
		t.Fatalf("field of a nil map = %q, want empty", got)
	}
	end = EndState{Fields: map[string]string{"result": "ok"}}
	if got := end.Field("result"); got != "ok" {
		t.Fatalf("field = %q, want ok", got)
	}
	if got := end.Field("missing"); got != "" {
		t.Fatalf("missing field = %q, want empty", got)
	}
}
