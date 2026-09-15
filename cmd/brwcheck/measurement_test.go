package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestEveryMeasurementBudgetIsSet pins that each entry point has one, since a
// zero duration makes context.WithTimeout expire immediately rather than never.
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
			ctx, cancel := context.WithTimeout(context.Background(), testCase.budget)
			defer cancel()
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("no deadline was set")
			}
		})
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
