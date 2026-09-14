package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
		Observe json.RawMessage `json:"observe"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &req); err != nil {
			// The tool's own decode reports a malformed body; there is no
			// observe value to read out of one.
			return observer{level: browser.ObserveFull}, nil
		}
	}
	value, err := observeValue(req.Observe)
	if err != nil {
		return observer{}, err
	}
	level, explicit, err := browser.ParseObserveLevel(value)
	if err != nil {
		return observer{}, err
	}
	return observer{level: level, explicit: explicit}, nil
}

// observeValue reads the parameter as a string. A present-but-non-string value
// is refused the same way an unrecognised string is: JSON has four other types
// a caller can send, and decoding straight into a string field would take
// {"observe":123}, {"observe":true} and {"observe":["none"]} for "absent" and
// widen each of them back to full without a word.
func observeValue(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", fmt.Errorf("unknown observe level %s; supported: %s",
			truncateForError(string(trimmed)), strings.Join(browser.ObserveLevels(), ", "))
	}
	return value, nil
}

// truncateForError keeps a rejected value short enough to read: it is echoed
// back to the caller, and the caller chose how long it was.
func truncateForError(value string) string {
	const limit = 48
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

// action trims an action result to the requested level and hands it to toolJSON.
// At the default level the result is returned unchanged, so a call that omits
// observe is byte-identical to one made before the parameter existed.
func (o observer) action(result browser.ActionResult, err error) (any, *rpcError) {
	return toolJSON(o.level.ApplyToAction(result), err)
}

// navigation is action for a tool whose outcome is the destination. It keeps
// url at every level; see browser.ObserveLevel.ApplyToNavigation.
func (o observer) navigation(result browser.ActionResult, err error) (any, *rpcError) {
	return toolJSON(o.level.ApplyToNavigation(result), err)
}

// batch trims a batch's single closing observation.
func (o observer) batch(result browser.BatchResult, err error) (any, *rpcError) {
	return toolJSON(o.level.ApplyToBatch(result), err)
}

// plan trims a plan's per-step observations: the last step reports at the
// caller's level and the intermediate ones drop to minimal, because a plan's
// intermediate observations are read by nobody — the next step has already run.
// This is the one tool whose default response differs from before the parameter
// existed, which is why observePlanSchema() states the split; a step's own
// product (a snapshot, a read) is not an observation and survives every level.
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

// wantSnapshot arms the post-action page snapshot, and refuses the one
// parameter pair that contradicts itself: snapshot:true asks for the page and
// observe minimal/none delete it on the way out. The caller set both
// deliberately and cannot see which one brw honoured, so it is named rather
// than resolved silently.
func (o observer) wantSnapshot(ctx context.Context, want bool) (context.Context, error) {
	if !want {
		return ctx, nil
	}
	if o.level != browser.ObserveFull {
		return ctx, fmt.Errorf("snapshot:true cannot be combined with observe:%q, which drops the snapshot it asks for; use observe:\"full\" or drop snapshot", o.level)
	}
	return browser.WithWantSnapshot(ctx), nil
}

// requireFindAction refuses a level a read-only brw_find cannot honour. The
// match list IS that call's payload: minimal and none have nothing to trim, so
// honouring them would answer nothing and ignoring them would make the
// parameter a silent no-op on the tool's most common call.
func (o observer) requireFindAction() error {
	if o.level == browser.ObserveFull {
		return nil
	}
	return fmt.Errorf("observe:%q applies to the post-action observation and brw_find without action returns the match list; pass action to locate and act, or observe:\"full\"", o.level)
}

// observeSchema is the shared parameter description. It is repeated on every
// action tool, so it is kept short: the catalogue is re-sent every turn, and the
// longer form belongs in the agent system prompt and docs/agent-guide.md. It
// still has to say that none does not skip the look, or a caller will read it as
// a latency control and be wrong.
func observeSchema() map[string]any {
	return stringEnumSchema("How much post-action observation to return. full (default): outcome, url, title, focus, "+
		"changed summary and the frontier element list. minimal: the same without the element list. none: the outcome "+
		"alone. brw observes at every level, so this saves tokens, not time. Refused with snapshot:true, which asks for "+
		"the page minimal and none drop.",
		browser.ObserveLevels()...)
}

// observeBatchSchema is brw_batch's own, because a batch reports ONE closing
// observation and it carries no element list: there is nothing for minimal to
// drop, so advertising the shared text here would promise a saving this tool
// cannot make.
func observeBatchSchema() map[string]any {
	return stringEnumSchema("How much of the single closing observation to return. full (default): outcome, url, title, "+
		"focus and the changed summary. minimal is the SAME as full here — a batch's closing observation carries no "+
		"element list to drop. none: the outcome alone. Per-step results are kept at every level. brw observes at every "+
		"level, so this saves tokens, not time.",
		browser.ObserveLevels()...)
}

// observePlanSchema is brw_plan's own, because a plan reports one observation
// PER STEP and applies a default split across them, and because a step's own
// product is not an observation: the snapshot a `snapshot` step fetched is the
// reason the step was written, so no level deletes it.
func observePlanSchema() map[string]any {
	return stringEnumSchema("How much post-action observation to return per step. Omit it and intermediate steps report "+
		"minimal while the last reports full, which is the split that makes a plan cheap. An explicit level applies to "+
		"every step: full is outcome, url, title, focus, changed summary and the frontier element list; minimal is the "+
		"same without the element list; none is the outcome alone. A snapshot or read step keeps what it fetched at "+
		"every level. brw observes at every level, so this saves tokens, not time.",
		browser.ObserveLevels()...)
}

// observeFindSchema is brw_find's own, because the parameter is only real when
// action turns the search into a locate-and-act. On a read-only find the match
// list is the whole answer, so minimal and none are refused by name instead of
// being accepted and ignored.
func observeFindSchema() map[string]any {
	return stringEnumSchema("How much post-action observation to return in result when action is set. full (default): "+
		"outcome, url, title, focus, changed summary and the frontier element list. minimal: the same without the "+
		"element list. none: the outcome alone. matched is kept at every level. Refused on a read-only find, where the "+
		"match list is the answer and there is nothing to trim.",
		browser.ObserveLevels()...)
}

// observeToolNames are the tools that read the observe parameter. The catalogue
// test checks it against the advertised schemas in BOTH directions, and a
// behavioural test calls every name on it, so neither an unadvertised reader
// nor an advertised no-op can survive.
func observeToolNames() []string {
	return []string{
		"brw_click", "brw_click_text", "brw_type", "brw_fill", "brw_select", "brw_press",
		"brw_scroll", "brw_hover", "brw_navigate", "brw_navigate_to", "brw_drag",
		"brw_mouse_down", "brw_mouse_up", "brw_focus", "brw_find", "brw_batch", "brw_plan",
	}
}

// navigationToolNames are the tools whose result message names the url that was
// REQUESTED, so their observation has to keep the one that was committed.
func navigationToolNames() []string {
	return []string{"brw_navigate", "brw_navigate_to"}
}
