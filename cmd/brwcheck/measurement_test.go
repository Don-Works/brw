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

const perModeBudget = 15 * time.Minute

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

var errEvalStub = errors.New("stub suite")

type evalCall struct {
	ran         bool
	modes       []agenteval.Mode
	hasDeadline bool
	remaining   time.Duration
}

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

func TestTheBudgetCoversTheModesTheRunDrives(t *testing.T) {
	
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
