package agenteval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Don-Works/brw/internal/agenteval/solver"
	"github.com/Don-Works/brw/internal/harness"
)

// ReportSchema names the shape of the machine-readable report.
const ReportSchema = "brw.agenteval/v1"

// Options configures a run.
type Options struct {
	RepoRoot   string
	ChromePath string
	// Only restricts the run to one task id.
	Only string
	// Modes selects which solver behaviours to run. Empty means honest only.
	Modes []Mode
	// Judge is the optional LLM layer. Nil runs the deterministic check alone,
	// which is the default and needs no key and no network.
	Judge *Judge
	// Timeout bounds a single browser operation.
	Timeout time.Duration
}

// Result is one task run in one mode.
type Result struct {
	Task    string `json:"task"`
	Title   string `json:"title"`
	Mode    Mode   `json:"mode"`
	Fixture string `json:"fixture"`

	Passed  bool     `json:"passed"`
	Reasons []string `json:"reasons,omitempty"`
	// Expected is what this mode is supposed to produce: an honest run passes, a
	// sabotaged one fails. A harness whose sabotaged run passes is broken, and
	// this is the field that says so.
	Expected   bool `json:"expected_pass"`
	AsExpected bool `json:"as_expected"`

	ClaimedSuccess         bool   `json:"claimed_success"`
	ClaimedWithoutReaching bool   `json:"claimed_without_reaching"`
	Answer                 string `json:"answer,omitempty"`
	FailureReport          string `json:"failure_report,omitempty"`

	EndState   EndState      `json:"end_state"`
	DurationMS int64         `json:"duration_ms"`
	Error      string        `json:"error,omitempty"`
	Judge      *JudgeVerdict `json:"judge,omitempty"`
}

// Report is a whole run.
type Report struct {
	Schema      string              `json:"schema"`
	StartedAt   time.Time           `json:"started_at"`
	DurationMS  int64               `json:"duration_ms"`
	Environment harness.Environment `json:"environment"`
	// Judge names the model the optional layer used, or says the run was graded
	// by the deterministic check alone.
	Judge      string   `json:"judge"`
	Results    []Result `json:"results"`
	AsExpected int      `json:"as_expected"`
	Surprises  int      `json:"surprises"`
	OK         bool     `json:"ok"`
}

// Run drives the selected tasks in the selected modes.
func Run(ctx context.Context, opts Options) (Report, error) {
	root, err := filepath.Abs(opts.RepoRoot)
	if err != nil {
		return Report{}, err
	}
	tasks, err := selectTasks(opts.Only)
	if err != nil {
		return Report{}, err
	}
	modes := opts.Modes
	if len(modes) == 0 {
		modes = []Mode{ModeHonest}
	}

	fixtures, err := harness.ServeFixtures(root)
	if err != nil {
		return Report{}, err
	}
	defer fixtures.Close()
	if err := fixtures.Reachable(tasks[0].Fixture); err != nil {
		return Report{}, fmt.Errorf("fixture origin: %w", err)
	}
	digest, err := fixtures.Digest()
	if err != nil {
		return Report{}, err
	}

	started := time.Now()
	rig, err := harness.LaunchBrowser(ctx, harness.BrowserOptions{
		ChromePath: opts.ChromePath,
		Timeout:    opts.Timeout,
	})
	if err != nil {
		return Report{}, err
	}
	defer rig.Close()

	environment := harness.DescribeEnvironment()
	environment.Browser = rig.Version.Browser
	environment.CDPProtocol = rig.Version.Protocol
	environment.Headless = true
	environment.FixtureDigest = digest

	report := Report{
		Schema:      ReportSchema,
		StartedAt:   started.UTC(),
		Environment: environment,
		Judge:       "deterministic end-state check only",
	}
	if opts.Judge != nil {
		report.Judge = "deterministic end-state check plus " + opts.Judge.Model
	}

	for _, task := range tasks {
		for _, mode := range modes {
			result := runTask(ctx, rig, fixtures, task, mode, opts.Judge)
			report.Results = append(report.Results, result)
			if result.AsExpected {
				report.AsExpected++
			} else {
				report.Surprises++
			}
		}
	}
	report.OK = report.Surprises == 0 && len(report.Results) > 0
	report.DurationMS = time.Since(started).Milliseconds()
	return report, nil
}

func selectTasks(only string) ([]Task, error) {
	all := Tasks()
	only = strings.TrimSpace(only)
	if only == "" {
		return all, nil
	}
	task, ok := TaskByID(only)
	if !ok {
		ids := make([]string, 0, len(all))
		for _, candidate := range all {
			ids = append(ids, candidate.ID)
		}
		return nil, fmt.Errorf("unknown task %q; have %s", only, strings.Join(ids, ", "))
	}
	return []Task{task}, nil
}

// runTask opens a fresh tab, lets the solver work, then reads the end state out
// of the page and grades it.
//
// The order matters: the probe runs after the solver has stopped and reads the
// page directly, so what it reports is not something the solver handed over.
// The return value is named so the deferred duration lands on what the caller
// receives rather than on a copy already made.
func runTask(ctx context.Context, rig *harness.Browser, fixtures *harness.Fixtures, task Task, mode Mode, judge *Judge) (result Result) {
	result = Result{
		Task:     task.ID,
		Title:    task.Title,
		Mode:     mode,
		Fixture:  task.Fixture,
		Expected: mode == ModeHonest,
	}
	started := time.Now()
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()

	url := fixtures.URL(task.Fixture)
	opened, err := rig.Manager.Open(ctx, url)
	if err != nil {
		result.Error = err.Error()
		result.AsExpected = false
		return result
	}
	tabID := opened.Tab.ID
	defer func() { _ = rig.Manager.CloseTab(ctx, tabID) }()

	agent := solver.New(ctx, rig.Manager, tabID)
	outcome, solveErr := task.Solve(ctx, agent, mode)
	if solveErr != nil {
		result.Error = solveErr.Error()
	}
	result.ClaimedSuccess = outcome.ClaimedSuccess
	result.Answer = outcome.Answer
	result.FailureReport = outcome.FailureReport

	probe := &Probe{ctx: ctx, manager: rig.Manager, tabID: tabID, url: url}
	end, probeErr := task.Probe(ctx, probe)
	result.EndState = end
	if probeErr != nil {
		// Without an end state there is nothing to grade, and the safe answer to
		// "did it work" with no evidence is no.
		result.Passed = false
		result.Reasons = []string{"could not read the end state: " + probeErr.Error()}
		result.AsExpected = result.Passed == result.Expected
		return result
	}

	verdict := task.Check(end, outcome)
	result.Passed = verdict.Passed
	result.Reasons = verdict.Reasons
	result.ClaimedWithoutReaching = verdict.ClaimedWithoutReaching

	if judge != nil {
		judged, err := judge.Grade(ctx, task, end)
		if err != nil {
			judged = JudgeVerdict{Model: judge.Model, Error: err.Error()}
		}
		result.Judge = &judged
		result.Passed, result.Reasons = applyJudge(result.Passed, result.Reasons, judged)
	}

	result.AsExpected = result.Passed == result.Expected
	return result
}

// applyJudge folds the optional layer into the deterministic verdict.
//
// The judge can fail a run the end-state check passed. It cannot pass one the
// check failed: the page is the evidence, and a model's reading of the page
// does not outrank the harness reading it. A judge that could not answer leaves
// the verdict alone, so the harness stays runnable while the API is down.
func applyJudge(passed bool, reasons []string, judged JudgeVerdict) (bool, []string) {
	if judged.Error != "" || !passed || judged.Passed {
		return passed, reasons
	}
	reason := judged.Reason
	if reason == "" {
		reason = "no reason given"
	}
	return false, append(reasons, "judge: "+reason)
}

// WriteJSON emits the machine-readable report.
func (r Report) WriteJSON(w io.Writer) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(r)
}

// WriteSummary emits the human form.
func (r Report) WriteSummary(w io.Writer) {
	fmt.Fprintf(w, "brw agent evaluation %s\n", r.Schema)
	fmt.Fprintf(w, "environment: %s\n", r.Environment.Fingerprint())
	fmt.Fprintf(w, "graded by:   %s\n\n", r.Judge)

	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "TASK\tMODE\tGRADE\tEXPECTED\tMS\tNOTE")
	for _, result := range r.Results {
		note := ""
		if result.ClaimedWithoutReaching {
			note = "claimed success without reaching the end state"
		}
		if result.Error != "" && note == "" {
			note = "error: " + result.Error
		}
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\t%s\n",
			result.Task, result.Mode, passFail(result.Passed), passFail(result.Expected), result.DurationMS, note)
	}
	_ = table.Flush()

	for _, result := range r.Results {
		if len(result.Reasons) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s (%s) did not reach the end state:\n", result.Task, result.Mode)
		for _, reason := range result.Reasons {
			fmt.Fprintf(w, "  - %s\n", reason)
		}
	}

	fmt.Fprintf(w, "\n%d of %d runs graded as expected\n", r.AsExpected, len(r.Results))
	if !r.OK {
		fmt.Fprintln(w, "EVALUATION FAILED — a run did not grade the way its mode requires")
	}
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}
