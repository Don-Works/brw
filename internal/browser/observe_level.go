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

// ApplyToBatch trims a batch's single closing observation. The per-step results
// are untouched at every level: they are the record of what ran, and a batch
// that hid which step failed would be unusable.
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
// An explicit level applies to every step, including the last, so a caller that
// asked for none gets none.
func (l ObserveLevel) ApplyToPlan(result PlanResult, explicit bool) PlanResult {
	if len(result.Steps) == 0 {
		return result
	}
	last := len(result.Steps) - 1
	for i := range result.Steps {
		level := PlanStepObserveLevel(l, explicit, i, len(result.Steps))
		if level == ObserveFull {
			continue
		}
		result.Steps[i].Result = trimStepResult(result.Steps[i].Result, level)
		if i != last || level == ObserveNone {
			result.Steps[i].Snapshot = nil
		}
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

// observationOnlyKeys are the ActionResult fields an observe level drops. They
// are listed by wire name so a plan step result that came back over HTTP as a
// generic object trims to the same shape as one produced in-process.
var observationOnlyKeys = map[ObserveLevel][]string{
	ObserveMinimal: {"elements", "targets", "snapshot"},
	ObserveNone:    {"elements", "targets", "snapshot", "changed", "url", "title", "focus", "version"},
}

// trimStepResult trims one plan step payload. A step result is typed as any
// because a plan carries read/snapshot payloads too; only an action observation
// is trimmed, in whichever of its two shapes arrived — the concrete struct from
// an in-process transport, or the decoded object from the upstream HTTP one.
func trimStepResult(value any, level ObserveLevel) any {
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
		if !looksLikeObservation(typed) {
			return value
		}
		for _, key := range observationOnlyKeys[level] {
			delete(typed, key)
		}
		return typed
	default:
		return value
	}
}

// looksLikeObservation keeps the map branch from eating a step payload that is
// not an action observation (a read, a structured-data extraction). Every
// ActionResult carries ok and message; a read carries neither together.
func looksLikeObservation(value map[string]any) bool {
	if _, ok := value["ok"]; !ok {
		return false
	}
	_, hasElements := value["elements"]
	_, hasChanged := value["changed"]
	_, hasURL := value["url"]
	return hasElements || hasChanged || hasURL
}
