// Package solver is the surface an evaluation's solver acts through: the
// semantic verbs brw exposes to an agent over MCP, and nothing else.
//
// It is a package rather than a type in internal/agenteval because the
// constraint it carries is only real across a package boundary. The grader
// reads its evidence by evaluating script in the page; a solver that could
// reach the same channel could arrange for the evidence to agree with it. Agent
// holds the browser manager in an unexported field, so a task body — which
// lives in internal/agenteval, not here — cannot take it back out. The one way
// back in is a wait condition carrying JavaScript, which WaitFor refuses.
package solver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
)

// Agent is what a task's Solve is handed.
type Agent struct {
	ctx     context.Context
	manager *browser.Manager
	tabID   string
}

// New binds an agent to one tab. Only the harness calls it: a solver is given
// an Agent and has no manager of its own to build another with.
func New(ctx context.Context, manager *browser.Manager, tabID string) *Agent {
	return &Agent{ctx: ctx, manager: manager, tabID: tabID}
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
