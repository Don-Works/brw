package extensionbridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

func (b *Bridge) ExecuteBatch(ctx context.Context, steps []browser.BatchStep) (browser.BatchResult, error) {
	if err := browser.GuardCrossOriginRefs("batch", browser.BridgeCrossOriginRemedy, browser.BatchStepRefs(steps)...); err != nil {
		return browser.BatchResult{}, err
	}
	// Keep the caller context for the final observation so a cancelled run can
	// still report current page state; step execution uses the cancel-aware ctx.
	obsCtx := ctx
	entry, release := b.cancels.register(ctx, cancelToken(ctx, ""))
	defer release()
	ctx = entry.ctx

	// Resolve the active tab ONCE for the whole sequence and pin it into the
	// step context, so each step's contextTabID() short-circuits instead of
	// re-issuing get_active_tab_id per step (3-11x per step otherwise). A
	// focus_tab / open step legitimately moves the active tab, so we re-pin after
	// any retargeting step (see retargetPinnedTab).
	stepCtx := b.pinActiveTab(ctx)

	result := browser.BatchResult{OK: true, Steps: make([]browser.BatchStepResult, 0, len(steps)), TabID: browser.TabIDFromContext(stepCtx)}
	for i, step := range steps {
		if entry.Cancelled() {
			result.Cancelled = true
			result.OK = false
			result.Error = "cancelled"
			break
		}
		sr, retargetTo := b.executeBatchStep(stepCtx, i, step)
		stepCtx = b.retargetPinnedTab(ctx, stepCtx, retargetTo)
		result.Steps = append(result.Steps, sr)
		if !sr.OK {
			if entry.Cancelled() {
				result.Cancelled = true
				result.OK = false
				result.Error = "cancelled"
				result.Steps = result.Steps[:len(result.Steps)-1]
				break
			}
			result.OK = false
			result.Error = sr.Error
			break
		}
	}
	result.StepsCompleted = len(result.Steps)
	// Pin the observation to the tab the sequence ended on (the step pin, which
	// already tracked focus_tab/open moves) so the closing snapshot resolves the
	// active tab once via the pinned id instead of re-issuing get_active_tab_id
	// across its tryCached/store/evaluate sub-calls. Falls back to obsCtx when no
	// tab could be pinned, preserving the prior per-call behaviour.
	if pinned := browser.TabIDFromContext(stepCtx); pinned != "" {
		obsCtx = browser.WithTabID(obsCtx, pinned)
		// Refresh the reported tab id to the tab the sequence actually ended on
		// (focus_tab/open retargets), so a batch that moved focus does not return
		// the initial tab_id alongside the new tab's URL/title.
		result.TabID = pinned
	} else {
		obsCtx = b.pinActiveTab(obsCtx)
		if p := browser.TabIDFromContext(obsCtx); p != "" {
			result.TabID = p
		}
	}
	snap, snapErr := b.Snapshot(obsCtx, snapshot.SnapshotOptions{ViewportOnly: true})
	if snapErr == nil {
		result.URL = snap.URL
		result.Title = snap.Title
		if snap.Metadata != nil {
			if v, ok := snap.Metadata["version"].(float64); ok {
				result.Version = int64(v)
			}
			if focus, ok := snap.Metadata["focused_ref"].(string); ok {
				result.Focus = focus
			}
		}
		frontier := browser.SelectFrontierElements(snap.Elements, result.Focus, 12)
		result.Changed = browser.SummarizeElements(frontier, 12)
	} else if result.Error == "" {
		result.OK = false
		result.Error = "final batch observation failed: " + snapErr.Error()
	}
	return result, nil
}

// executeBatchStep runs one batch step and returns its result plus the retarget
// target tab id — the KNOWN id of the tab a successful focus_tab/open moved focus
// to ("" for any other step or on failure), used by the loop to re-pin without
// reading the mutable active-tab cache.
func (b *Bridge) executeBatchStep(ctx context.Context, index int, step browser.BatchStep) (browser.BatchStepResult, string) {
	sr := browser.BatchStepResult{Index: index, Action: step.Action, OK: true}
	var actionErr error
	retargetTo := ""

	// Site consent, re-checked against where the tab is NOW: the batch was gated
	// once from arguments that stopped being true as soon as a step navigated.
	if err := browser.GateSequenceStep(ctx, index, b.contextTabID(ctx), step.ConsentProbe()); err != nil {
		sr.OK = false
		sr.Error = err.Error()
		return sr, retargetTo
	}
	switch step.Action {
	case "click":
		if step.Ref == "" {
			actionErr = errors.New("click requires ref")
			break
		}
		beforeTabs := b.captureTabIDs(ctx)
		sourceTabID := b.contextTabID(ctx)
		actionErr = b.clickRef(ctx, step.Ref)
		b.settle(ctx, batchActionSettle)
		if actionErr == nil && beforeTabs != nil {
			if tabs, err := b.ListTabs(ctx); err == nil {
				sr.NewTabID = openedChildTabID(tabs, beforeTabs, sourceTabID)
			}
		}
	case "click_text":
		if step.Text == "" {
			actionErr = errors.New("click_text requires text")
			break
		}
		_, actionErr = b.clickTextRaw(ctx, snapshot.ClickTextOptions{Text: step.Text})
		b.settle(ctx, batchActionSettle)
	case "find_act":
		// Locate and act in one step. The search must resolve to exactly one
		// element or the step fails, so a batch can never act on the
		// highest-ranked of several rivals.
		if step.Find == nil {
			actionErr = errors.New("find_act requires find")
			break
		}
		var findRef string
		findRef, actionErr = browser.RunFindActStep(ctx, b, b.findActuator(), *step.Find)
		sr.Ref = findRef
		b.settle(ctx, batchActionSettle)
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		actionErr = b.typeRef(ctx, step.Ref, step.Text)
		b.settle(ctx, batchActionSettle)
	case "fill":
		_, actionErr = b.fillOptions(ctx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Value: step.Value, Replace: true})
		b.settle(ctx, batchActionSettle)
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		_, actionErr = b.selectValue(ctx, step.Ref, step.Value)
		b.settle(ctx, batchActionSettle)
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		actionErr = b.pressKey(ctx, step.Key)
		b.settle(ctx, batchActionSettle)
	case "scroll":
		_, actionErr = b.scrollDirection(ctx, step.Direction)
		b.settle(ctx, batchActionSettle)
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		actionErr = b.hoverRef(ctx, step.Ref)
		b.settle(ctx, batchActionSettle)
	case "wait":
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 10 * time.Second
		}
		actionErr = b.WaitFor(ctx, step.Condition, timeout)
	case "open":
		if step.URL == "" {
			actionErr = errors.New("open requires url")
			break
		}
		var openRes browser.OpenResult
		openRes, actionErr = b.Open(ctx, step.URL)
		if actionErr == nil {
			actionErr = openRes.NavigationErr()
		}
		if actionErr == nil {
			retargetTo = openRes.Tab.ID
		}
	case "navigate_to":
		// Keep batch parity with the plan and direct-CDP runners. This reuses the
		// batch's pinned working tab; unlike open it must not create or retarget to
		// another tab.
		if step.URL == "" {
			actionErr = errors.New("navigate_to requires url")
			break
		}
		var url string
		url, actionErr = b.prepareNavigationURL(step.URL)
		if actionErr == nil {
			actionErr = b.navigateToURLAndWait(ctx, url)
		}
		if actionErr == nil {
			b.settle(ctx, observedActionSettle)
		}
	case "focus_tab":
		if step.ID == "" {
			actionErr = errors.New("focus_tab requires id")
			break
		}
		actionErr = b.FocusTab(ctx, step.ID)
		if actionErr == nil {
			retargetTo = step.ID
		}
	case "assert_visible":
		if step.Ref == "" {
			actionErr = errors.New("assert_visible requires ref")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertVisible(ctx, step.Ref, timeout)
	case "assert_text":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("assert_text requires ref and text")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertText(ctx, step.Ref, step.Text, timeout)
	case "assert_value":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("assert_value requires ref and value")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertValue(ctx, step.Ref, step.Value, timeout)
	case "assert_hidden":
		if step.Ref == "" {
			actionErr = errors.New("assert_hidden requires ref")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertHidden(ctx, step.Ref, timeout)
	case "assert":
		// Deliberately has no timeout, matching the direct-CDP runner: these
		// assertions read current state once. A batch that needs the page to
		// settle first puts a wait step in front of the assertion, where the
		// wait is visible in the replayable flow.
		if step.Assertion == nil {
			actionErr = errors.New("assert requires assertion")
			break
		}
		_, actionErr = browser.Assert(ctx, b, *step.Assertion)
	default:
		actionErr = fmt.Errorf("unknown action %q", step.Action)
	}
	if actionErr != nil {
		sr.OK = false
		sr.Error = actionErr.Error()
	}
	if sr.OK && retargetTo != "" {
		sr.TabID = retargetTo
		if step.Action == "open" {
			sr.NewTabID = retargetTo
		}
	}
	return sr, retargetTo
}
