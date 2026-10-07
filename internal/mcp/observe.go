package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
)

type observer struct {
	level    browser.ObserveLevel
	explicit bool
}

func observerFromArgs(args json.RawMessage) (observer, error) {
	var req struct {
		Observe json.RawMessage `json:"observe"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &req); err != nil {

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

func truncateForError(value string) string {
	const limit = 48
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func (o observer) action(result browser.ActionResult, err error) (any, *rpcError) {
	return toolJSON(o.level.ApplyToAction(result), err)
}

func (o observer) navigation(result browser.ActionResult, err error) (any, *rpcError) {
	return toolJSON(o.level.ApplyToNavigation(result), err)
}

func (o observer) batch(result browser.BatchResult, err error) (any, *rpcError) {
	return toolJSON(o.level.ApplyToBatch(result), err)
}

func (o observer) plan(result browser.PlanResult, err error) (any, *rpcError) {
	return toolJSON(o.level.ApplyToPlan(result, o.explicit), err)
}

func (o observer) findAct(result browser.FindActResult, err error) (any, *rpcError) {
	result.Result = o.level.ApplyToAction(result.Result)
	return toolJSON(result, err)
}

func (o observer) wantSnapshot(ctx context.Context, want bool) (context.Context, error) {
	if !want {
		return ctx, nil
	}
	if o.level != browser.ObserveFull {
		return ctx, fmt.Errorf("snapshot:true cannot be combined with observe:%q, which drops the snapshot it asks for; use observe:\"full\" or drop snapshot", o.level)
	}
	return browser.WithWantSnapshot(ctx), nil
}

func (o observer) requireFindAction() error {
	if o.level == browser.ObserveFull {
		return nil
	}
	return fmt.Errorf("observe:%q applies to the post-action observation and brw_find without action returns the match list; pass action to locate and act, or observe:\"full\"", o.level)
}

func observeSchema() map[string]any {
	return stringEnumSchema("How much post-action observation to return. full (default): outcome, url, title, focus, "+
		"changed summary and the frontier element list. minimal: the same without the element list. none: the outcome "+
		"alone. brw observes at every level, so this saves tokens, not time. Refused with snapshot:true, which asks for "+
		"the page minimal and none drop.",
		browser.ObserveLevels()...)
}

func observeBatchSchema() map[string]any {
	return stringEnumSchema("How much of the single closing observation to return. full (default): outcome, url, title, "+
		"focus and the changed summary. minimal is the SAME as full here — a batch's closing observation carries no "+
		"element list to drop. none: the outcome alone. Per-step results are kept at every level. brw observes at every "+
		"level, so this saves tokens, not time.",
		browser.ObserveLevels()...)
}

func observePlanSchema() map[string]any {
	return stringEnumSchema("How much post-action observation to return per step. Omit it and intermediate steps report "+
		"minimal while the last reports full, which is the split that makes a plan cheap. An explicit level applies to "+
		"every step: full is outcome, url, title, focus, changed summary and the frontier element list; minimal is the "+
		"same without the element list; none is the outcome alone. A snapshot or read step keeps what it fetched at "+
		"every level. brw observes at every level, so this saves tokens, not time.",
		browser.ObserveLevels()...)
}

func observeFindSchema() map[string]any {
	return stringEnumSchema("How much post-action observation to return in result when action is set. full (default): "+
		"outcome, url, title, focus, changed summary and the frontier element list. minimal: the same without the "+
		"element list. none: the outcome alone. matched is kept at every level. Refused on a read-only find, where the "+
		"match list is the answer and there is nothing to trim.",
		browser.ObserveLevels()...)
}

func observeToolNames() []string {
	return []string{
		"brw_click", "brw_click_text", "brw_type", "brw_fill", "brw_select", "brw_press",
		"brw_scroll", "brw_hover", "brw_navigate", "brw_navigate_to", "brw_drag",
		"brw_mouse_down", "brw_mouse_up", "brw_focus", "brw_find", "brw_batch", "brw_plan",
	}
}
