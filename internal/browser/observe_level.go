package browser

import (
	"fmt"
	"strings"
)

// ObserveLevel selects how much of the post-action observation is REPORTED.
//
// It never selects whether brw looks. The observation is also where the
// navigation policy re-checks the committed destination and where the tab's
// semantic-state version advances, so a level that skipped the read would let a
// caller opt out of a guard by asking for a smaller answer. Every level costs
// the same round trip; what changes is how many tokens the answer spends.
type ObserveLevel string

const (
	// ObserveFull is the default and today's payload: outcome, URL, title, focus,
	// what changed, and the frontier element list with refs to act on next.
	ObserveFull ObserveLevel = "full"
	// ObserveMinimal keeps the outcome, where the page now is, and the summary of
	// what changed, and drops the element list. Enough to confirm a step landed,
	// not enough to pick the next ref without a snapshot.
	ObserveMinimal ObserveLevel = "minimal"
	// ObserveNone reports only the outcome: did the action succeed, did anything
	// change, and any warning. An action whose outcome is unknowable is not a
	// saving, so success/failure and changed_state survive every level.
	ObserveNone ObserveLevel = "none"
)

// ObserveLevels lists the accepted values for schemas and error messages.
func ObserveLevels() []string {
	return []string{string(ObserveFull), string(ObserveMinimal), string(ObserveNone)}
}

// ParseObserveLevel maps a caller's value onto a level. An empty value means the
// caller did not ask, which is reported separately so a runner can apply its own
// default without having to distinguish it from an explicit "full".
func ParseObserveLevel(value string) (level ObserveLevel, explicit bool, err error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return ObserveFull, false, nil
	}
	switch ObserveLevel(trimmed) {
	case ObserveFull, ObserveMinimal, ObserveNone:
		return ObserveLevel(trimmed), true, nil
	}
	return ObserveFull, false, fmt.Errorf("unknown observe level %q; supported: %s", value, strings.Join(ObserveLevels(), ", "))
}

// ApplyToAction trims an action result to the level. ObserveFull returns the
// result unchanged, so a caller that passes nothing gets byte-identical output.
func (l ObserveLevel) ApplyToAction(result ActionResult) ActionResult {
	switch l {
	case ObserveMinimal:
		result.Elements = nil
		result.Targets = nil
		result.Snapshot = nil
		return result
	case ObserveNone:
		result.Elements = nil
		result.Targets = nil
		result.Snapshot = nil
		result.Changed = nil
		result.URL = ""
		result.Title = ""
		result.Focus = ""
		result.Version = 0
		return result
	default:
		return result
	}
}

// ApplyToNavigation trims a navigation's observation and keeps url at every
// level, because a navigation's outcome IS the destination.
//
// The message is written from the url that was REQUESTED, before the
// observation reads the one the browser committed to. A result that kept that
// message and dropped the observed url would assert a destination brw never
// verified: after a redirect, an interstitial or a login wall the caller would
// be told it arrived somewhere it did not.
func (l ObserveLevel) ApplyToNavigation(result ActionResult) ActionResult {
	committed := result.URL
	trimmed := l.ApplyToAction(result)
	trimmed.URL = committed
	return trimmed
}

// ApplyToBatch trims a batch's single closing observation. The per-step results
// are untouched at every level: they are the record of what ran, and a batch
// that hid which step failed would be unusable.
//
// ObserveMinimal is deliberately the same as ObserveFull here. A BatchResult
// carries no element list to drop — the whole point of a batch is one closing
// observation — so minimal has nothing to trim, and brw_batch's own schema text
// says so rather than advertising a saving that cannot happen.
func (l ObserveLevel) ApplyToBatch(result BatchResult) BatchResult {
	switch l {
	case ObserveMinimal:
		return result
	case ObserveNone:
		result.Changed = nil
		result.URL = ""
		result.Title = ""
		result.Focus = ""
		result.Version = 0
		return result
	default:
		return result
	}
}

// ApplyToPlan trims a plan's per-step observations. The LAST step keeps the
// caller's level (defaulting to full) and the intermediate steps drop to
// minimal, because a plan's intermediate observations are the ones nobody reads:
// the flow has already committed to the next step before the model sees them.
//
// An explicit level applies to every step's OBSERVATION, including the last, so
// a caller that asked for none gets none. It never touches a step's own
// product: a `snapshot` or `read` step was written to fetch that payload, and a
// level that deleted it would turn the step into a round trip that returns
// nothing. SKILL.md sends agents to brw_plan for exactly that mid-flow
// snapshot.
//
// The input is not modified: the trim writes into copies, so a caller that logs
// or re-reads the untrimmed result still sees what the runner produced.
func (l ObserveLevel) ApplyToPlan(result PlanResult, explicit bool) PlanResult {
	if len(result.Steps) == 0 {
		return result
	}
	steps := make([]PlanStepResult, len(result.Steps))
	copy(steps, result.Steps)
	result.Steps = steps
	for i := range steps {
		level := PlanStepObserveLevel(l, explicit, i, len(steps))
		if level == ObserveFull {
			continue
		}
		steps[i].Result = trimStepResult(steps[i].Action, steps[i].Result, level)
	}
	return result
}

// PlanStepObserveLevel is the level one plan step reports at.
func PlanStepObserveLevel(level ObserveLevel, explicit bool, index, total int) ObserveLevel {
	if explicit {
		return level
	}
	if index == total-1 {
		return ObserveFull
	}
	return ObserveMinimal
}

// planStepPayload says what a plan step's result carries, which is what decides
// whether an observe level may trim it.
type planStepPayload int

const (
	// planStepProduct is the step's own product — a snapshot, a page read, the
	// tab an open created. The caller wrote the step to get it, so no level
	// drops it. It is the zero value on purpose: an unclassified verb reports in
	// full rather than losing a payload nobody classified.
	planStepProduct planStepPayload = iota
	// planStepObservation is a post-action observation, the payload observe
	// exists to trim.
	planStepObservation
	// planStepFindAct is a locate-and-act result: {matched, action, result},
	// with the observation nested one level down and the matched element — the
	// answer to "which one did you act on" — beside it.
	planStepFindAct
)

// planStepPayloads classifies every brw_plan step verb. The trim is driven by
// the verb the caller wrote rather than by sniffing the payload's shape: the
// shape differs by transport (a typed struct in-process, a decoded object over
// the upstream HTTP proxy), and the predicate that guessed from shape trimmed
// neither find_act form. TestEveryPlanStepVerbIsClassifiedForObserve checks
// this table against the advertised step enum in both directions, so a new verb
// cannot quietly default its own level.
var planStepPayloads = map[string]planStepPayload{
	"click":       planStepObservation,
	"click_text":  planStepObservation,
	"type":        planStepObservation,
	"fill":        planStepObservation,
	"select":      planStepObservation,
	"press":       planStepObservation,
	"scroll":      planStepObservation,
	"hover":       planStepObservation,
	"navigate_to": planStepObservation,
	"find_act":    planStepFindAct,
	"snapshot":    planStepProduct,
	"read":        planStepProduct,
	"open":        planStepProduct,
	"wait":        planStepProduct,
	"focus_tab":   planStepProduct,
}

// PlanStepVerbIsClassified reports whether a plan step verb has an entry in the
// observe classification. Exported for the catalogue test that checks the table
// against the advertised step enum from the package that owns that enum.
func PlanStepVerbIsClassified(action string) bool {
	_, ok := planStepPayloads[action]
	return ok
}

// ClassifiedPlanStepVerbs lists the verbs the observe classification knows, so
// the same test can catch a table entry for a verb no longer advertised.
func ClassifiedPlanStepVerbs() []string {
	verbs := make([]string, 0, len(planStepPayloads))
	for verb := range planStepPayloads {
		verbs = append(verbs, verb)
	}
	return verbs
}

// observationOnlyKeys are the ActionResult fields an observe level drops. They
// are listed by wire name so a plan step result that came back over HTTP as a
// generic object trims to the same shape as one produced in-process.
var observationOnlyKeys = map[ObserveLevel][]string{
	ObserveMinimal: {"elements", "targets", "snapshot"},
	ObserveNone:    {"elements", "targets", "snapshot", "changed", "url", "title", "focus", "version"},
}

// trimStepResult trims one plan step payload to the level its verb allows.
func trimStepResult(action string, value any, level ObserveLevel) any {
	switch planStepPayloads[action] {
	case planStepObservation:
		return trimObservation(value, level)
	case planStepFindAct:
		return trimFindActResult(value, level)
	default:
		return value
	}
}

// trimObservation trims an action observation in whichever of its two shapes
// arrived: the concrete struct from an in-process transport, or the decoded
// object from the upstream HTTP one.
func trimObservation(value any, level ObserveLevel) any {
	switch typed := value.(type) {
	case ActionResult:
		return level.ApplyToAction(typed)
	case *ActionResult:
		if typed == nil {
			return value
		}
		trimmed := level.ApplyToAction(*typed)
		return &trimmed
	case map[string]any:
		return withoutKeys(typed, observationOnlyKeys[level])
	default:
		return value
	}
}

// trimFindActResult trims the observation half of a locate-and-act step and
// leaves `matched` alone: which element was chosen is the answer, not the
// observation, and a step whose ref is gone cannot be followed up.
func trimFindActResult(value any, level ObserveLevel) any {
	switch typed := value.(type) {
	case FindActResult:
		typed.Result = level.ApplyToAction(typed.Result)
		return typed
	case *FindActResult:
		if typed == nil {
			return value
		}
		trimmed := *typed
		trimmed.Result = level.ApplyToAction(trimmed.Result)
		return &trimmed
	case map[string]any:
		nested, ok := typed["result"]
		if !ok {
			return value
		}
		out := withoutKeys(typed, nil)
		out["result"] = trimObservation(nested, level)
		return out
	default:
		return value
	}
}

// withoutKeys copies a decoded payload minus the named keys. It copies rather
// than deleting in place because the map belongs to the result the runner
// produced, which the caller may still read or log untrimmed.
func withoutKeys(in map[string]any, keys []string) map[string]any {
	drop := make(map[string]bool, len(keys))
	for _, key := range keys {
		drop[key] = true
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		if drop[key] {
			continue
		}
		out[key] = value
	}
	return out
}
