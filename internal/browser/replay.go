package browser

import (
	"fmt"
	"sort"
	"strings"
)

// ReplayResult is a recorded flow expressed as brw_batch steps.
//
// Claude in Chrome answers repeatability with a GIF a human watches and with
// shortcuts an LLM re-reads and re-performs. Both put a model back in the replay
// loop, so a replay can silently diverge from what was recorded. brw writes
// every action to its trace together with the ref and the semantic identity of
// the element it acted on, so it can hand back something stricter: the same
// flow as deterministic batch steps, re-runnable in one call, with no model
// involved, and a diff a human can review.
type ReplayResult struct {
	Steps []BatchStep `json:"steps"`
	Count int         `json:"count"`
	// Actions is how many steps perform an action, as opposed to guarding one.
	Actions int `json:"actions"`
	// Guards is how many assert steps were inserted ahead of an action to check
	// that its ref still points at the element the recording acted on.
	Guards int `json:"guards"`
	// Unguarded names the actions whose element identity could not be checked,
	// because the recorded role does not carry its accessible name as visible
	// text. Those steps replay on ref alone.
	Unguarded []string `json:"unguarded,omitempty"`
	// TabSwitches counts the focus_tab steps inserted because the recorded flow
	// crossed tabs. Without them every step would run in whichever tab the batch
	// started in.
	TabSwitches int `json:"tab_switches,omitempty"`
	// Skipped counts trace entries that are not replayable, with the reasons.
	// Dropping them silently would make a partial replay look complete.
	Skipped int            `json:"skipped"`
	Reasons map[string]int `json:"skipped_reasons,omitempty"`
	// Failed counts exported steps that did not succeed when recorded. They are
	// still exported — the flow is what the agent actually did — but anyone
	// building a regression script wants to know before re-running it.
	Failed int    `json:"failed_steps,omitempty"`
	Note   string `json:"note,omitempty"`
}

// ReplayOptions controls how a trace is turned into batch steps.
type ReplayOptions struct {
	// Guards inserts an assert step before each action whose element identity
	// can be checked. Defaults on: a replay that clicks the wrong element in
	// silence is worse than one that fails.
	Guards *bool
	// IncludeFailed keeps steps whose recorded action failed. Defaults on, so
	// the export is a faithful record; set false to export only what worked.
	IncludeFailed *bool
}

func (o ReplayOptions) guards() bool {
	return o.Guards == nil || *o.Guards
}

func (o ReplayOptions) includeFailed() bool {
	return o.IncludeFailed == nil || *o.IncludeFailed
}

// replayableActions maps a traced action onto the brw_batch verb that
// reproduces it. Anything absent cannot be expressed as a batch step.
var replayableActions = map[string]string{
	"click":       "click",
	"click_text":  "click_text",
	"type":        "type",
	"fill":        "fill",
	"select":      "select",
	"press":       "press",
	"scroll":      "scroll",
	"hover":       "hover",
	"navigate_to": "navigate_to",
}

// textBearingRoles are the roles whose accessible name is also their visible
// text, which is what assert_text compares against (it reads innerText, then
// textContent, then value). Guarding a textbox on its aria-label would assert
// against an empty string and fail every time, so those roles go unguarded
// rather than getting a check that cannot pass.
var textBearingRoles = map[string]bool{
	"button":   true,
	"link":     true,
	"tab":      true,
	"menuitem": true,
	"option":   true,
	"heading":  true,
}

// TraceToBatch converts recorded actions into brw_batch steps.
//
// Coordinate-driven entries (click_button, drag, the raw mouse verbs) are
// skipped rather than exported: they were recorded as pixel positions, and
// replaying a pixel position against a page that has since re-laid-out clicks
// somewhere else entirely. Refs self-heal across re-renders; coordinates do not.
func TraceToBatch(trace TraceResult, opts ReplayOptions) ReplayResult {
	out := ReplayResult{
		Steps:   make([]BatchStep, 0, len(trace.Entries)),
		Reasons: map[string]int{},
	}
	unguarded := map[string]bool{}
	currentTab := ""

	for _, entry := range trace.Entries {
		verb, ok := replayableActions[entry.Action]
		if !ok {
			out.Skipped++
			out.Reasons[skipReason(entry.Action)]++
			continue
		}
		if !entry.OK && !opts.includeFailed() {
			out.Skipped++
			out.Reasons["action failed when recorded"]++
			continue
		}
		step, ok := batchStepFor(verb, entry)
		if !ok {
			out.Skipped++
			out.Reasons[missingOperandReason(verb, entry)]++
			continue
		}

		// A ref only means something within the tab that issued it. Without an
		// explicit focus_tab, a flow recorded across two tabs replays entirely
		// in whichever tab the batch happens to start in — and since a textbox
		// carries no guard, it does so silently.
		if entry.TabID != "" && entry.TabID != currentTab {
			if currentTab != "" {
				out.Steps = append(out.Steps, BatchStep{Action: "focus_tab", ID: entry.TabID})
				out.TabSwitches++
			}
			currentTab = entry.TabID
		}

		if guard, ok := guardStepFor(entry); opts.guards() && ok {
			out.Steps = append(out.Steps, guard)
			out.Guards++
		} else if opts.guards() && entry.Ref != "" {
			unguarded[describeUnguarded(entry)] = true
		}

		if !entry.OK {
			out.Failed++
		}
		out.Steps = append(out.Steps, step)
		out.Actions++
	}

	out.Count = len(out.Steps)
	out.Unguarded = sortedKeys(unguarded)
	if len(out.Reasons) == 0 {
		out.Reasons = nil
	}
	out.Note = replayNote(out, opts)
	return out
}

// guardStepFor builds the assert step that checks a ref still points at the
// element the recording acted on, or reports that no meaningful check exists.
//
// assert_text compares against innerText, then textContent, then value. A guard
// is therefore only emitted when the element's accessible name is actually
// present as text: an icon-only <button aria-label="Save"> has the name "Save"
// and the role button, but no text at all, so a guard on it could never pass and
// would fail every replay of an unchanged page.
func guardStepFor(entry TraceEntry) (BatchStep, bool) {
	name := strings.TrimSpace(entry.Name)
	if entry.Ref == "" || name == "" {
		return BatchStep{}, false
	}
	if !textBearingRoles[strings.ToLower(strings.TrimSpace(entry.Role))] {
		return BatchStep{}, false
	}
	if !entry.NameIsVisibleText {
		return BatchStep{}, false
	}
	return BatchStep{Action: "assert_text", Ref: entry.Ref, Text: name}, true
}

func describeUnguarded(entry TraceEntry) string {
	role := strings.TrimSpace(entry.Role)
	if role == "" {
		role = "element"
	}
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		return fmt.Sprintf("%s (%s)", entry.Ref, role)
	}
	return fmt.Sprintf("%s (%s %q)", entry.Ref, role, name)
}

func batchStepFor(verb string, entry TraceEntry) (BatchStep, bool) {
	step := BatchStep{Action: verb}
	switch verb {
	case "click", "hover":
		if entry.Ref == "" {
			return BatchStep{}, false
		}
		step.Ref = entry.Ref
	case "click_text":
		if entry.Text == "" {
			return BatchStep{}, false
		}
		step.Text = entry.Text
	case "type", "fill":
		// An empty value is never exported. Both executors accept a fill with no
		// text and CLEAR the field, so guessing here would make a replay
		// destructive rather than merely incomplete.
		if entry.Ref == "" || entry.Text == "" {
			return BatchStep{}, false
		}
		step.Ref = entry.Ref
		step.Text = entry.Text
	case "select":
		// Selecting a placeholder option with an empty value is legitimate, but
		// both batch executors reject a select without one, so it cannot be
		// replayed as a batch step.
		if entry.Ref == "" || entry.Value == "" {
			return BatchStep{}, false
		}
		step.Ref = entry.Ref
		step.Value = entry.Value
	case "press":
		if entry.Value == "" {
			return BatchStep{}, false
		}
		step.Key = entry.Value
	case "scroll":
		step.Direction = entry.Value
		if step.Direction == "" {
			step.Direction = "down"
		}
	case "navigate_to":
		if entry.Text == "" {
			return BatchStep{}, false
		}
		step.URL = entry.Text
	default:
		return BatchStep{}, false
	}
	return step, true
}

// skipReason groups skipped entries into something an agent can act on, rather
// than listing every raw action name back at it.
func skipReason(action string) string {
	switch action {
	case "click_button", "drag", "mouse_down", "mouse_up", "click_xy":
		return "coordinate-driven action, not ref-addressable, so not replayable"
	case "navigate":
		return "history navigation (back/forward/reload) depends on session history"
	default:
		if strings.TrimSpace(action) == "" {
			return "unnamed action"
		}
		return "not a replayable action: " + action
	}
}

func replayNote(result ReplayResult, opts ReplayOptions) string {
	notes := make([]string, 0, 3)
	if result.Actions == 0 {
		notes = append(notes, "no replayable actions in the trace; act by ref (click/type/fill/select) to record a replayable flow")
	}
	if len(result.Unguarded) > 0 {
		notes = append(notes, fmt.Sprintf("%d action(s) replay on ref alone because their role carries no visible text to check", len(result.Unguarded)))
	}
	if result.Failed > 0 {
		notes = append(notes, fmt.Sprintf("%d exported step(s) failed when recorded", result.Failed))
	}
	if !opts.guards() && result.Actions > 0 {
		notes = append(notes, "guards disabled: a replay will act on refs without checking what they now point at")
	}
	return strings.Join(notes, "; ")
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// missingOperandReason says which operand was absent, so an agent can tell a
// backend that did not record the value from an action that never had one.
func missingOperandReason(verb string, entry TraceEntry) string {
	if entry.Redacted {
		return "value withheld: the field was credential-bearing, so it was never recorded"
	}
	switch verb {
	case "type", "fill":
		if entry.Ref != "" && entry.Text == "" {
			return "recorded action did not capture the value that was entered"
		}
	case "select":
		if entry.Ref != "" && entry.Value == "" {
			return "recorded action did not capture the option that was selected"
		}
	}
	return "recorded action had no replayable target"
}
