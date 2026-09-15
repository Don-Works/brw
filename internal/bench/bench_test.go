package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/harness"
	"github.com/Don-Works/brw/internal/mcp"
)

// TestEveryFlowIsWellFormed walks the suite table rather than a list written
// beside it. A flow naming a fixture that does not exist, or a command labelled
// with a tool nobody can call, only fails minutes into a run otherwise — and
// the tool label is what makes the record mean anything to a reader.
func TestEveryFlowIsWellFormed(t *testing.T) {
	catalogue := map[string]bool{}
	for _, name := range mcp.ToolNames() {
		catalogue[name] = true
	}
	if len(catalogue) == 0 {
		t.Fatal("the MCP catalogue reported no tools; this test cannot check anything")
	}

	seen := map[string]bool{}
	for _, def := range flowDefs {
		t.Run(def.id, func(t *testing.T) {
			if def.id == "" {
				t.Fatal("flow has no id")
			}
			if seen[def.id] {
				t.Fatalf("duplicate flow id %q", def.id)
			}
			seen[def.id] = true

			path := filepath.Join("..", "..", "tests", "fixtures", def.fixture)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("flow %s names fixture %s: %v", def.id, def.fixture, err)
			}

			commands := def.build(&flowRunner{rig: &harness.Browser{}, refs: map[string]string{}})
			if len(commands) == 0 {
				t.Fatalf("flow %s drives no commands", def.id)
			}
			names := map[string]bool{}
			for index, cmd := range commands {
				if cmd.name == "" {
					t.Errorf("flow %s command %d has no name", def.id, index)
				}
				if names[cmd.name] {
					t.Errorf("flow %s repeats command name %q; the record would have two rows nothing distinguishes", def.id, cmd.name)
				}
				names[cmd.name] = true
				if cmd.run == nil {
					t.Errorf("flow %s command %q does nothing", def.id, cmd.name)
				}
				if !catalogue[cmd.tool] {
					t.Errorf("flow %s command %q is labelled %q, which is not a tool in the MCP catalogue", def.id, cmd.name, cmd.tool)
				}
			}
			if commands[0].name != "open" {
				t.Errorf("flow %s starts with %q; a flow that never opens its fixture measures the wrong page", def.id, commands[0].name)
			}
		})
	}
}

// TestBenchFixturesReachNothingOffThisMachine backs the claim the harness makes
// about itself. A fixture that pulls one stylesheet from a CDN turns a latency
// measurement into a measurement of somebody else's network, and it does so
// silently: the run still passes, the numbers are just wrong.
//
// warmupFixture is listed with the flow fixtures because it is opened by every
// run even though nothing reports on it.
func TestBenchFixturesReachNothingOffThisMachine(t *testing.T) {
	paths := []string{filepath.Join("..", "..", "tests", "fixtures", warmupFixture)}
	for _, def := range flowDefs {
		paths = append(paths, filepath.Join("..", "..", "tests", "fixtures", def.fixture))
	}
	if err := harness.AuditFixtures(paths); err != nil {
		t.Fatal(err)
	}
}

func TestSelectFlows(t *testing.T) {
	all, err := selectFlows("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(flowDefs) {
		t.Fatalf("empty filter selected %d flows, want all %d", len(all), len(flowDefs))
	}

	for _, id := range FlowIDs() {
		one, err := selectFlows(id)
		if err != nil {
			t.Fatalf("select %q: %v", id, err)
		}
		if len(one) != 1 || one[0].id != id {
			t.Fatalf("select %q returned %d flows", id, len(one))
		}
	}

	if _, err := selectFlows("does-not-exist"); err == nil {
		t.Fatal("an unknown flow id was accepted")
	} else if !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("error %q does not name the unknown flow", err)
	}
}

func TestEstimateTokensMatchesThePublishedEstimator(t *testing.T) {
	cases := []struct {
		bytes int
		want  int
	}{
		{bytes: 0, want: 0},
		{bytes: -5, want: 0},
		{bytes: 3, want: 0},
		{bytes: 4, want: 1},
		{bytes: 4951, want: 1237},
	}
	for _, testCase := range cases {
		if got := EstimateTokens(testCase.bytes); got != testCase.want {
			t.Errorf("EstimateTokens(%d) = %d, want %d", testCase.bytes, got, testCase.want)
		}
	}
}

func TestObservationBytesMeasuresTheJSONAnAgentReceives(t *testing.T) {
	if got := ObservationBytes(nil); got != 0 {
		t.Errorf("nil observation = %d bytes, want 0", got)
	}
	payload := map[string]string{"ok": "true"}
	if got := ObservationBytes(payload); got != len(`{"ok":"true"}`) {
		t.Errorf("observation = %d bytes, want %d", got, len(`{"ok":"true"}`))
	}
	// A value JSON cannot represent must not abort a measurement of a command
	// that did in fact run.
	if got := ObservationBytes(make(chan int)); got != 0 {
		t.Errorf("unmarshalable observation = %d bytes, want 0", got)
	}
}

func TestTotalsAccumulate(t *testing.T) {
	var totals Totals
	totals.AddCommand(Command{WallMS: 10.5, CDPCommands: 2, CDPMessages: 3, TransportBytesTx: 100, TransportBytesRx: 200, ObservationBytes: 40, ObservationTokens: 10})
	totals.AddCommand(Command{WallMS: 1.5, CDPCommands: 1, CDPMessages: 1, TransportBytesTx: 10, TransportBytesRx: 20, ObservationBytes: 4, ObservationTokens: 1})
	if totals.Commands != 2 || totals.WallMS != 12 || totals.CDPCommands != 3 || totals.CDPMessages != 4 {
		t.Fatalf("command totals wrong: %+v", totals)
	}
	if totals.TransportBytesTx != 110 || totals.TransportBytesRx != 220 || totals.ObservationBytes != 44 || totals.ObservationTokens != 11 {
		t.Fatalf("byte totals wrong: %+v", totals)
	}

	var run Totals
	run.AddTotals(totals)
	run.AddTotals(totals)
	if run.Commands != 4 || run.WallMS != 24 || run.TransportBytesRx != 440 {
		t.Fatalf("run totals wrong: %+v", run)
	}
}

func TestSummaryLeadsWithTheFingerprintAndMarksAFailedRun(t *testing.T) {
	record := Record{
		Schema: RecordSchema,
		Environment: harness.Environment{
			OS: "linux", Arch: "amd64", CPUModel: "Fixture CPU", CPUs: 4,
			Browser: "Chrome/1.2.3", BrwVersion: "0.1.0", GoVersion: "go1.26.0",
			FixtureDigest: "deadbeefcafe0000",
		},
		Flows: []Flow{{
			ID: "forms", Fixture: "forms.html",
			Commands: []Command{{Name: "open", Tool: "brw_open", WallMS: 12.3, OK: true}},
			Totals:   Totals{Commands: 1, WallMS: 12.3},
			Error:    "click_submit: no such element",
		}},
		Totals: Totals{Commands: 1, WallMS: 12.3},
		OK:     false,
	}

	var out strings.Builder
	record.WriteSummary(&out)
	text := out.String()

	fingerprintAt := strings.Index(text, "Fixture CPU")
	numbersAt := strings.Index(text, "12.3")
	if fingerprintAt < 0 || numbersAt < 0 {
		t.Fatalf("summary missing the fingerprint or the numbers:\n%s", text)
	}
	if fingerprintAt > numbersAt {
		t.Errorf("summary prints numbers before the environment they were measured in:\n%s", text)
	}
	for _, want := range []string{"brw_open", "click_submit: no such element", "RUN FAILED"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary missing %q:\n%s", want, text)
		}
	}
}

func TestSummarySaysWhenSystemMetricsAreUnavailable(t *testing.T) {
	record := Record{Schema: RecordSchema, OK: true}
	var out strings.Builder
	record.WriteSummary(&out)
	if !strings.Contains(out.String(), "system metrics: unavailable") {
		t.Fatalf("a record with no system metrics printed no such note:\n%s", out.String())
	}
}
