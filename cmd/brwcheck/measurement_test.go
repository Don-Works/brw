package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/agenteval"
)

// TestMeasurementRunsCarryAWholeRunDeadline reads the two entry points rather
// than driving them, because the thing being guarded is a wedge: a browser that
// never answers the launch handshake, or a wait nothing times out, and neither
// can be provoked without hanging this test for as long as the bug would hang
// the run.
//
// The per-operation Timeout the entry points pass bounds one browser call, not
// the run, so context.Background() on its own means `task bench` hangs with no
// output and nothing to read.
func TestMeasurementRunsCarryAWholeRunDeadline(t *testing.T) {
	const root = "context.Background()"
	const wrapper = "context.WithTimeout("

	for _, name := range []string{"bench.go", "agenteval.go"} {
		t.Run(name, func(t *testing.T) {
			source, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(source, []byte(wrapper+root)) {
				t.Fatalf("%s starts no bounded context; a wedged run hangs with no output", name)
			}
			for offset := 0; ; {
				index := bytes.Index(source[offset:], []byte(root))
				if index < 0 {
					return
				}
				at := offset + index
				if at < len(wrapper) || string(source[at-len(wrapper):at]) != wrapper {
					t.Errorf("%s reaches %s at byte %d without a deadline around it", name, root, at)
				}
				offset = at + len(root)
			}
		})
	}
}

// perModeBudget is the room one pass over the whole suite is given. It is
// written here rather than divided out of evalBudget so that the check below
// weighs the deadline a run actually gets against a number, not against the
// constant the run was built from.
const perModeBudget = 15 * time.Minute

// TestEveryMeasurementBudgetIsSet pins that each entry point has one, since a
// zero duration makes context.WithTimeout expire immediately rather than never
// — `task bench` would then fail before it launched anything, and the failure
// would read as a browser problem.
//
// This checks the constants only. That the entry points run under them is
// TestMeasurementRunsCarryAWholeRunDeadline's job, and which one an evaluation
// picks is TestTheBudgetCoversTheModesTheRunDrives'.
func TestEveryMeasurementBudgetIsSet(t *testing.T) {
	cases := []struct {
		name   string
		budget time.Duration
	}{
		{name: "bench", budget: benchBudget},
		{name: "agent eval", budget: evalBudget},
		{name: "agent eval verify", budget: verifyBudget},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.budget <= time.Minute {
				t.Fatalf("budget is %s; a measurement that takes seconds needs headroom, not a hair trigger", testCase.budget)
			}
		})
	}
}

// errEvalStub stands in for whatever the suite would have returned. It is an
// error so that runAgentEval returns before printing a report, and a sentinel
// so a test can tell "the suite ran" from "runAgentEval failed earlier".
var errEvalStub = errors.New("stub suite")

// evalCall records what runAgentEval handed the suite runner.
type evalCall struct {
	ran         bool
	modes       []agenteval.Mode
	hasDeadline bool
	remaining   time.Duration
}

// driveAgentEval runs the real entry point with the suite runner swapped out,
// and reports the modes and the deadline it was actually given. The entry
// point is what is under test: re-asking evalPlanFor what the budget should
// have been cannot notice runAgentEval building its context from a different
// constant, which is the mistake the pairing exists to prevent.
func driveAgentEval(t *testing.T, verify bool) *evalCall {
	t.Helper()
	call := &evalCall{}
	previous := runEvalSuite
	t.Cleanup(func() { runEvalSuite = previous })
	runEvalSuite = func(ctx context.Context, opts agenteval.Options) (agenteval.Report, error) {
		call.ran = true
		call.modes = opts.Modes
		if deadline, ok := ctx.Deadline(); ok {
			call.hasDeadline = true
			call.remaining = time.Until(deadline)
		}
		return agenteval.Report{}, errEvalStub
	}
	if err := runAgentEval(evalOptions{RepoRoot: t.TempDir(), Verify: verify}); !errors.Is(err, errEvalStub) {
		t.Fatalf("runAgentEval returned %v, want %v; the suite was never reached", err, errEvalStub)
	}
	if !call.ran {
		t.Fatal("the suite runner was never called")
	}
	return call
}

// TestTheBudgetCoversTheModesTheRunDrives drives runAgentEval and reads the
// deadline it puts on the context against the modes it puts in the options,
// because the two are picked off the same flag and nothing else makes the
// budget grow when the mode list does. A verify run given the honest-only
// budget is killed midway, and every task it never reached is reported as
// failing rather than as never attempted.
//
// It also pins which run drives the sabotaged task. Dropping it from
// --eval-verify leaves a run that passes everything, which is the state the
// flag exists to rule out, and one a mode count alone would not notice.
func TestTheBudgetCoversTheModesTheRunDrives(t *testing.T) {
	// startupSlack is the time between the context being created and the
	// stub reading it. It is microseconds; a second is generous.
	const startupSlack = time.Second

	cases := []struct {
		name      string
		verify    bool
		sabotaged bool
	}{
		{name: "an ordinary run", verify: false, sabotaged: false},
		{name: "a verify run", verify: true, sabotaged: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			call := driveAgentEval(t, testCase.verify)
			if len(call.modes) == 0 {
				t.Fatal("no modes; the run drives nothing")
			}
			if got := slices.Contains(call.modes, agenteval.ModeSabotaged); got != testCase.sabotaged {
				t.Errorf("drives the sabotaged task = %t, want %t (modes %v)", got, testCase.sabotaged, call.modes)
			}
			if !call.hasDeadline {
				t.Fatal("the suite was given a context with no deadline; a wedged run hangs with no output")
			}
			want := perModeBudget * time.Duration(len(call.modes))
			if call.remaining+startupSlack < want {
				t.Errorf("the run has %s left for %d mode(s), want at least %s; it is cut off partway and the tasks it never reached are reported as failures",
					call.remaining.Round(time.Second), len(call.modes), want)
			}
		})
	}
}

// TestEveryEvaluationModeIsReachable enumerates the modes the package offers
// and requires each to be driven by some run, so that a mode added later is
// covered without editing this test. A mode nothing selects is dead grading
// code that still compiles and silently never runs.
//
// The modes come from driving the entry point rather than from evalPlanFor, so
// a runAgentEval that overrides the plan with a list of its own fails here.
func TestEveryEvaluationModeIsReachable(t *testing.T) {
	driven := map[agenteval.Mode]string{}
	for _, verify := range []bool{false, true} {
		name := "an ordinary run"
		if verify {
			name = "a verify run"
		}
		for _, mode := range driveAgentEval(t, verify).modes {
			if _, seen := driven[mode]; !seen {
				driven[mode] = name
			}
		}
	}
	for _, mode := range agenteval.Modes() {
		if _, ok := driven[mode]; !ok {
			t.Errorf("mode %q is never driven; no flag selects it", mode)
		}
	}
	for mode, by := range driven {
		if !slices.Contains(agenteval.Modes(), mode) {
			t.Errorf("%s drives mode %q, which is not one agenteval offers", by, mode)
		}
	}
}

// TestMeasurementTasksStampAVersion guards the documented entry points against
// producing records stamped "brw dev".
//
// Environment.Comparable deliberately leaves brw_version out of what it
// compares, on the grounds that holding one brw version against another is the
// point of keeping records. That cannot happen if every record made through
// `task bench` says dev, which is what a bare `go run` produces.
func TestMeasurementTasksStampAVersion(t *testing.T) {
	taskfile, err := os.ReadFile(filepath.Join("..", "..", "Taskfile.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for _, line := range strings.Split(string(taskfile), "\n") {
		if !strings.Contains(line, "./cmd/brwcheck") {
			continue
		}
		found++
		if !strings.Contains(line, `-ldflags "{{.GO_LDFLAGS}}"`) {
			t.Errorf("this line builds brwcheck without the version:\n  %s", strings.TrimSpace(line))
		}
	}
	if found == 0 {
		t.Fatal("no Taskfile line runs brwcheck; this test cannot check anything")
	}
}
