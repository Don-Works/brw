package browser

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ObserveLevel selects how much of the post-action observation is REPORTED.
type ObserveLevel string

const (
	// ObserveFull is the default and today's payload: outcome, URL, title, focus, what changed, and the frontier element list with refs to act on next.
	ObserveFull ObserveLevel = "full"
	// ObserveMinimal keeps the outcome, where the page now is, and the summary of what changed, and drops the element list.
	ObserveMinimal ObserveLevel = "minimal"
	// ObserveNone reports only the outcome: did the action succeed, did anything change, and any warning.
	ObserveNone ObserveLevel = "none"
)

// ObserveLevels lists the accepted values for schemas and error messages.
func ObserveLevels() []string {
	return []string{string(ObserveFull), string(ObserveMinimal), string(ObserveNone)}
}

// ParseObserveLevel maps a caller's value onto a level.
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

// ApplyToAction trims an action result to the level.
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

var navigationActions = []string{"navigate", "navigate_to"}

// NavigationActions lists those verbs.
func NavigationActions() []string {
	return slices.Clone(navigationActions)
}

// IsNavigationAction reports whether a verb's result is a navigation.
func IsNavigationAction(action string) bool {
	return slices.Contains(navigationActions, action)
}

// ApplyToNavigation trims a navigation's observation and keeps url at every level, because a navigation's outcome IS the destination.
func (l ObserveLevel) ApplyToNavigation(result ActionResult) ActionResult {
	committed := result.URL
	trimmed := l.ApplyToAction(result)
	trimmed.URL = committed
	return trimmed
}

// ApplyToBatch trims a batch's single closing observation.
func (l ObserveLevel) ApplyToBatch(result BatchResult) BatchResult {
	switch l {
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

// ApplyToPlan trims a plan's per-step observations.
func (l ObserveLevel) ApplyToPlan(result PlanResult, explicit bool) PlanResult {
	if len(result.Steps) == 0 {
		return result
	}
	steps := slices.Clone(result.Steps)
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

type planStepPayload int

const (
	planStepProduct planStepPayload = iota

	planStepObservation

	planStepFindAct

	planStepNavigation
)

var planStepPayloads = map[string]planStepPayload{
	"click":       planStepObservation,
	"click_text":  planStepObservation,
	"type":        planStepObservation,
	"fill":        planStepObservation,
	"select":      planStepObservation,
	"press":       planStepObservation,
	"scroll":      planStepObservation,
	"hover":       planStepObservation,
	"navigate_to": planStepNavigation,
	"find_act":    planStepFindAct,
	"snapshot":    planStepProduct,
	"read":        planStepProduct,
	"open":        planStepProduct,
	"wait":        planStepProduct,
	"focus_tab":   planStepProduct,
}

// PlanStepVerbIsClassified reports whether a plan step verb has an entry in the observe classification.
func PlanStepVerbIsClassified(action string) bool {
	_, ok := planStepPayloads[action]
	return ok
}

// ClassifiedPlanStepVerbs lists the verbs the observe classification knows, so the same test can catch a table entry for a verb no longer advertised.
func ClassifiedPlanStepVerbs() []string {
	return slices.Collect(maps.Keys(planStepPayloads))
}

// PlanStepVerbKeepsTheCommittedURL reports whether a plan step verb's result is trimmed as a navigation, so the package that owns the advertised step enum can check that classification against the tool surface without reaching into this table.
func PlanStepVerbKeepsTheCommittedURL(action string) bool {
	return planStepPayloads[action] == planStepNavigation
}

var observationOnlyKeys = map[ObserveLevel][]string{
	ObserveMinimal: {"elements", "targets", "snapshot"},
	ObserveNone:    {"elements", "targets", "snapshot", "changed", "url", "title", "focus", "version"},
}

func trimStepResult(action string, value any, level ObserveLevel) any {
	switch planStepPayloads[action] {
	case planStepObservation:
		return trimObservation(value, level)
	case planStepNavigation:
		return trimNavigation(value, level)
	case planStepFindAct:
		return trimFindActResult(value, level)
	default:
		return value
	}
}

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

func trimNavigation(value any, level ObserveLevel) any {
	switch typed := value.(type) {
	case ActionResult:
		return level.ApplyToNavigation(typed)
	case *ActionResult:
		if typed == nil {
			return value
		}
		trimmed := level.ApplyToNavigation(*typed)
		return &trimmed
	case map[string]any:
		return withoutKeys(typed, navigationOnlyKeys(level))
	default:
		return value
	}
}

func navigationOnlyKeys(level ObserveLevel) []string {
	return slices.DeleteFunc(slices.Clone(observationOnlyKeys[level]), func(key string) bool { return key == "url" })
}

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

func withoutKeys(in map[string]any, keys []string) map[string]any {
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	for _, key := range keys {
		delete(out, key)
	}
	return out
}
