package mcp

import (
	"encoding/json"

	"github.com/Don-Works/brw/internal/browser"
)

// observer carries one call's requested observation level plus whether the
// caller asked at all. The two are kept apart because a sequence runner defaults
// its intermediate steps to minimal, and it must be able to tell "the caller
// said full" from "the caller said nothing".
type observer struct {
	level    browser.ObserveLevel
	explicit bool
}

// observerFromArgs reads the shared observe parameter. An unknown value is an
// error rather than a silent fallback to full: a caller who asked for fewer
// tokens and got all of them would have no way to notice.
func observerFromArgs(args json.RawMessage) (observer, error) {
	var req struct {
		Observe string `json:"observe"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &req); err != nil {
			// The tool's own decode reports a malformed body; an observe value
			// that is not a string just means the caller did not ask for one here.
			return observer{level: browser.ObserveFull}, nil
		}
	}
	level, explicit, err := browser.ParseObserveLevel(req.Observe)
	if err != nil {
		return observer{}, err
	}
	return observer{level: level, explicit: explicit}, nil
}

// action trims an action result to the requested level and hands it to toolJSON.
// At the default level the result is returned unchanged, so a call that omits
// observe is byte-identical to one made before the parameter existed.
func (o observer) action(result browser.ActionResult, err error) (any, *rpcError) {
	return toolJSON(o.level.ApplyToAction(result), err)
}

// batch trims a batch's single closing observation.
func (o observer) batch(result browser.BatchResult, err error) (any, *rpcError) {
	return toolJSON(o.level.ApplyToBatch(result), err)
}

// plan trims a plan's per-step observations: the last step reports at the
// caller's level and the intermediate ones drop to minimal, because a plan's
// intermediate observations are read by nobody — the next step has already run.
func (o observer) plan(result browser.PlanResult, err error) (any, *rpcError) {
	return toolJSON(o.level.ApplyToPlan(result, o.explicit), err)
}

// findAct trims the observation half of a locate-and-act result, leaving the
// matched element alone: which element was chosen is the answer, not the
// observation, and dropping it would make the result unreadable.
func (o observer) findAct(result browser.FindActResult, err error) (any, *rpcError) {
	result.Result = o.level.ApplyToAction(result.Result)
	return toolJSON(result, err)
}

// observeSchema is the shared parameter description. It is repeated on every
// action tool, so it is kept short: the catalogue is re-sent every turn, and the
// longer form belongs in the agent system prompt and docs/agent-guide.md. It
// still has to say that none does not skip the look, or a caller will read it as
// a latency control and be wrong.
func observeSchema() map[string]any {
	return stringEnumSchema("How much post-action observation to return. full (default): outcome, url, title, focus, "+
		"changed summary and the frontier element list. minimal: the same without the element list. none: the outcome "+
		"alone. brw observes at every level, so this saves tokens, not time.",
		browser.ObserveLevels()...)
}

// observeToolNames are the tools that read the observe parameter. The catalogue
// test uses it so a tool can never read the parameter without advertising it.
func observeToolNames() []string {
	return []string{
		"brw_click", "brw_click_text", "brw_type", "brw_fill", "brw_select", "brw_press",
		"brw_scroll", "brw_hover", "brw_navigate", "brw_navigate_to", "brw_drag",
		"brw_mouse_down", "brw_mouse_up", "brw_focus", "brw_find", "brw_batch", "brw_plan",
	}
}
