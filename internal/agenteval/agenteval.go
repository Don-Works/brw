// Package agenteval measures what an agent ACHIEVES against the local fixture suite, rather than what one command returns.
package agenteval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Don-Works/brw/internal/agenteval/solver"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/harness"
)

// Mode selects how a task's solver behaves.
type Mode string

const (
	// ModeHonest solves the task.
	ModeHonest Mode = "honest"
	// ModeSabotaged skips the act that actually achieves the goal and reports success anyway.
	ModeSabotaged Mode = "sabotaged"
)

// Modes lists every mode, so a caller iterating them cannot miss one.
func Modes() []Mode { return []Mode{ModeHonest, ModeSabotaged} }

// Outcome is what the solver REPORTS about its own run.
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

// Field returns one observed value, or the empty string when the probe did not report it.
func (e EndState) Field(name string) string { return e.Fields[name] }

// Verdict is the deterministic grade.
type Verdict struct {
	Passed bool `json:"passed"`
	// Reasons names every criterion that did not hold.
	Reasons []string `json:"reasons,omitempty"`
	// ClaimedWithoutReaching is the interesting failure: the solver said it was done and the page says otherwise.
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
	// EndStateCriteria are the page-observable conditions for success.
	EndStateCriteria []string
	// Solve drives the browser and reports what it thinks it did.
	Solve func(context.Context, *solver.Agent, Mode) (Outcome, error)
	// Probe reads the end state out of the page, independently of the solver.
	Probe func(context.Context, *Probe) (EndState, error)
	// Check grades the end state.
	Check func(EndState, Outcome) Verdict
}

// Probe is the grader's surface.
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

// Observe evaluates an expression that must yield an object of strings and returns it as the end state.
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

func resolve(agent *solver.Agent, wanted map[string]harness.ElementQuery) (map[string]string, error) {
	page, err := agent.Snapshot()
	if err != nil {
		return nil, err
	}
	return harness.ResolveRefs(page.Elements, wanted)
}
