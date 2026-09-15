// Package agenteval measures what an agent ACHIEVES against the local fixture
// suite, rather than what one command returns.
//
// The shape that makes it an evaluation rather than a demo: the thing being
// graded reports its own outcome, and the grade comes from state the harness
// reads out of the page itself. A run that claims success without reaching the
// end state fails, which is checkable — see the sabotaged mode, which exists so
// the harness can be shown to fail.
//
// It runs with no API key. The deterministic end-state check is the whole
// verdict by default; an LLM judge is a layer over it that can only fail a run
// the check passed, never rescue one it failed. A harness that needs a key to
// run does not get run.
package agenteval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/harness"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
)

// Mode selects how a task's solver behaves.
type Mode string

const (
	// ModeHonest solves the task.
	ModeHonest Mode = "honest"
	// ModeSabotaged skips the act that actually achieves the goal and reports
	// success anyway. It is the control: an evaluation that cannot report this
	// as a failure is not measuring anything.
	ModeSabotaged Mode = "sabotaged"
)

// Modes lists every mode, so a caller iterating them cannot miss one.
func Modes() []Mode { return []Mode{ModeHonest, ModeSabotaged} }

// Outcome is what the solver REPORTS about its own run.
//
// It is not evidence. Only the task whose goal IS an accurate report reads it
// as part of the grade; for the rest the grade comes entirely from the page.
type Outcome struct {
	ClaimedSuccess bool   `json:"claimed_success"`
	Answer         string `json:"answer,omitempty"`
	FailureReport  string `json:"failure_report,omitempty"`
}

// EndState is what the harness read out of the page after the solver stopped.
type EndState struct {
	URL    string            `json:"url"`
	Fields map[string]string `json:"fields"`
}

// Field returns one observed value, or the empty string when the probe did not
// report it. An absent field is not a pass: every Check treats "" as "not
// observed", which is why a blank end state fails every task.
func (e EndState) Field(name string) string {
	if e.Fields == nil {
		return ""
	}
	return e.Fields[name]
}

// Verdict is the deterministic grade.
type Verdict struct {
	Passed bool `json:"passed"`
	// Reasons names every criterion that did not hold.
	Reasons []string `json:"reasons,omitempty"`
	// ClaimedWithoutReaching is the interesting failure: the solver said it was
	// done and the page says otherwise.
	ClaimedWithoutReaching bool `json:"claimed_without_reaching"`
}

// Task is one evaluation.
type Task struct {
	ID    string
	Title string
	// Fixture is the file under tests/fixtures the task runs against.
	Fixture string
	// Goal is the instruction, phrased as an agent would receive it.
	Goal string
	// EndStateCriteria are the page-observable conditions for success. They are
	// what the optional judge is shown, alongside the observed end state.
	EndStateCriteria []string
	// Solve drives the browser and reports what it thinks it did.
	Solve func(context.Context, *Agent, Mode) (Outcome, error)
	// Probe reads the end state out of the page, independently of the solver.
	Probe func(context.Context, *Probe) (EndState, error)
	// Check grades the end state.
	Check func(EndState, Outcome) Verdict
}

// Agent is the surface a solver acts through: the semantic verbs brw exposes
// to an agent over MCP.
//
// It has no way to run script in the page. That is deliberate and structural:
// the grader's evidence comes from evaluating script, and a solver that could
// reach the same channel could arrange for the evidence to agree with it. The
// one way back in is a wait condition carrying JavaScript, which WaitFor
// refuses.
type Agent struct {
	ctx     context.Context
	manager *browser.Manager
	tabID   string
}

func (a *Agent) context() context.Context {
	if a.tabID == "" {
		return a.ctx
	}
	return browser.WithTabID(a.ctx, a.tabID)
}

// Snapshot returns the page's semantic element list.
func (a *Agent) Snapshot() (snapshot.PageSnapshot, error) {
	return a.manager.Snapshot(a.context(), snapshot.SnapshotOptions{})
}

// Find locates elements live.
func (a *Agent) Find(opts snapshot.FindOptions) (snapshot.FindResult, error) {
	return a.manager.Find(a.context(), opts)
}

// FindOne resolves a single element and fails when the page offers none.
func (a *Agent) FindOne(opts snapshot.FindOptions) (snapshot.Element, error) {
	result, err := a.manager.Find(a.context(), opts)
	if err != nil {
		return snapshot.Element{}, err
	}
	if len(result.Elements) == 0 {
		return snapshot.Element{}, fmt.Errorf("no element matches role=%q text=%q", opts.Role, opts.Text)
	}
	return result.Elements[0], nil
}

// Fill writes into a field by ref.
func (a *Agent) Fill(ref, text string) error {
	_, err := a.manager.Fill(a.context(), snapshot.FillOptions{Ref: ref, Text: text, Replace: true})
	return err
}

// Click activates an element by ref.
func (a *Agent) Click(ref string) error {
	_, err := a.manager.Click(a.context(), ref)
	return err
}

// ClickText activates an element by its visible text.
func (a *Agent) ClickText(text, role string) error {
	_, err := a.manager.ClickText(a.context(), snapshot.ClickTextOptions{Text: text, Role: role})
	return err
}

// Select chooses an option in a listbox by ref.
func (a *Agent) Select(ref, value string) error {
	_, err := a.manager.Select(a.context(), ref, value)
	return err
}

// ScriptCarryingWaitPrefixes are the wait conditions that take JavaScript
// rather than a value to compare against.
//
// brw's wait grammar has one, and a solver allowed to use it would be back in
// the page with arbitrary script — the same channel the grader reads its
// evidence through. Exported so a test can check this list against brw's own
// in-page grammar instead of against a copy of it.
var ScriptCarryingWaitPrefixes = []string{"fn:"}

// WaitFor blocks until a page condition holds, refusing a condition that
// carries script. See ScriptCarryingWaitPrefixes.
func (a *Agent) WaitFor(condition string, timeout time.Duration) error {
	if prefix, refused := IsScriptCarryingCondition(condition); refused {
		return fmt.Errorf("a solver may not wait on %s, which runs script in the page", prefix)
	}
	return a.manager.WaitFor(a.context(), condition, timeout)
}

// IsScriptCarryingCondition reports whether a wait condition would execute
// JavaScript, and names the prefix that makes it so.
func IsScriptCarryingCondition(condition string) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(condition))
	for _, prefix := range ScriptCarryingWaitPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return prefix, true
		}
	}
	return "", false
}

// Read returns the page as an agent reads it.
func (a *Agent) Read() (readability.PageRead, error) {
	return a.manager.Read(a.context())
}

// Probe is the grader's surface. It reads the page directly and reports
// nothing but observed values.
type Probe struct {
	ctx     context.Context
	manager *browser.Manager
	tabID   string
	url     string
}

func (p *Probe) context() context.Context {
	if p.tabID == "" {
		return p.ctx
	}
	return browser.WithTabID(p.ctx, p.tabID)
}

// Observe evaluates an expression that must yield an object of strings and
// returns it as the end state. Every value is stringified in the page, so a
// missing element reports as the empty string rather than as a type error.
func (p *Probe) Observe(expression string) (EndState, error) {
	value, err := p.manager.Evaluate(p.context(), expression)
	if err != nil {
		return EndState{}, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return EndState{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return EndState{}, fmt.Errorf("probe expression did not yield an object: %w", err)
	}
	fields := make(map[string]string, len(raw))
	for key, item := range raw {
		switch typed := item.(type) {
		case string:
			fields[key] = typed
		case nil:
			fields[key] = ""
		default:
			fields[key] = fmt.Sprint(typed)
		}
	}
	return EndState{URL: p.url, Fields: fields}, nil
}

func fail(verdict *Verdict, format string, args ...any) {
	verdict.Reasons = append(verdict.Reasons, fmt.Sprintf(format, args...))
}

func settle(verdict Verdict, outcome Outcome) Verdict {
	verdict.Passed = len(verdict.Reasons) == 0
	verdict.ClaimedWithoutReaching = outcome.ClaimedSuccess && !verdict.Passed
	return verdict
}

func normalize(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// resolve captures the refs a solver acts through.
func resolve(agent *Agent, wanted map[string]harness.ElementQuery) (map[string]string, error) {
	page, err := agent.Snapshot()
	if err != nil {
		return nil, err
	}
	return harness.ResolveRefs(page.Elements, wanted)
}
