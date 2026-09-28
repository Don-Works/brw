package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
)

func (b *Bridge) ExecutePlan(ctx context.Context, steps []browser.PlanStep) (browser.PlanResult, error) {
	if err := browser.GuardCrossOriginRefs("plan", browser.BridgeCrossOriginRemedy, browser.PlanStepRefs(steps)...); err != nil {
		return browser.PlanResult{}, err
	}
	entry, release := b.cancels.register(ctx, cancelToken(ctx, ""))
	defer release()
	ctx = entry.ctx

	// Pin the tab once for the plan; retargetPinnedTab re-pins after focus_tab/open.
	stepCtx := b.pinActiveTab(ctx)

	result := browser.PlanResult{OK: true, Steps: make([]browser.PlanStepResult, 0, len(steps))}
	for i, step := range steps {
		if entry.Cancelled() {
			result.Cancelled = true
			result.OK = false
			result.Error = "cancelled"
			result.StepsCompleted = len(result.Steps)
			return result, nil
		}
		stepResult, retargetTo := b.executePlanStep(stepCtx, i, step)
		stepCtx = b.retargetPinnedTab(ctx, stepCtx, retargetTo)
		result.Steps = append(result.Steps, stepResult)
		if !stepResult.OK {
			if entry.Cancelled() {
				result.Cancelled = true
				result.OK = false
				result.Error = "cancelled"
				result.StepsCompleted = i
				return result, nil
			}
			result.OK = false
			failedAt := i
			result.FailedAt = &failedAt
			result.Error = stepResult.Error
			result.StepsCompleted = i
			return result, nil
		}
	}
	result.StepsCompleted = len(result.Steps)
	return result, nil
}

// executePlanStep returns the step result and the tab id a successful
// focus_tab/open moved to, or "".
func (b *Bridge) executePlanStep(ctx context.Context, index int, step browser.PlanStep) (browser.PlanStepResult, string) {
	sr := browser.PlanStepResult{Index: index, Action: step.Action, OK: true}
	retargetTo := ""

	// Re-check site consent against where the tab is NOW; an earlier step may
	// have navigated.
	if err := browser.GateSequenceStep(ctx, index, b.contextTabID(ctx), step.ConsentProbe()); err != nil {
		sr.OK = false
		sr.Error = err.Error()
		return sr, retargetTo
	}

	if step.ExpectRef != "" {
		findResult, err := b.Find(ctx, snapshot.FindOptions{Query: step.ExpectRef, Limit: 1})
		if err != nil {
			sr.OK = false
			sr.Error = fmt.Sprintf("expect_ref %q lookup failed: %v", step.ExpectRef, err)
			return sr, retargetTo
		}
		if len(findResult.Elements) == 0 {
			sr.OK = false
			sr.Error = fmt.Sprintf("expect_ref %q not found", step.ExpectRef)
			return sr, retargetTo
		}
		if step.ExpectRole != "" && findResult.Elements[0].Role != step.ExpectRole {
			sr.OK = false
			sr.Error = fmt.Sprintf("expect_ref %q has role %q, expected %q", step.ExpectRef, findResult.Elements[0].Role, step.ExpectRole)
			return sr, retargetTo
		}
	}

	var actionErr error
	switch step.Action {
	case "click":
		if step.Ref == "" {
			actionErr = errors.New("click requires ref")
			break
		}
		actionErr = b.clickRef(ctx, step.Ref)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "clicked " + step.Ref, "ref": step.Ref}
		}
		b.settle(ctx, batchActionSettle)
	case "find_act":
		// Several matches is an error, never a guess.
		if step.Find == nil {
			actionErr = errors.New("find_act requires find")
			break
		}
		var findRef string
		findRef, actionErr = browser.RunFindActStep(ctx, b, b.findActuator(), *step.Find)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "find_act " + step.Find.Action + " " + findRef, "ref": findRef}
		}
		b.settle(ctx, batchActionSettle)
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		actionErr = b.typeRef(ctx, step.Ref, step.Text)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "typed into " + step.Ref, "ref": step.Ref}
		}
		b.settle(ctx, batchActionSettle)
	case "fill":
		var ref string
		ref, actionErr = b.fillOptions(ctx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Value: step.Value, Replace: true})
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "filled " + ref, "ref": ref}
		}
		b.settle(ctx, batchActionSettle)
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		var message string
		message, actionErr = b.selectValue(ctx, step.Ref, step.Value)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": message, "ref": step.Ref, "value": step.Value}
		}
		b.settle(ctx, batchActionSettle)
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		actionErr = b.pressKey(ctx, step.Key)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "pressed " + step.Key, "key": step.Key}
		}
		b.settle(ctx, batchActionSettle)
	case "scroll":
		var message string
		message, actionErr = b.scrollDirection(ctx, step.Direction)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": message, "direction": step.Direction}
		}
		b.settle(ctx, batchActionSettle)
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		actionErr = b.hoverRef(ctx, step.Ref)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "hovered " + step.Ref, "ref": step.Ref}
		}
		b.settle(ctx, batchActionSettle)
	case "wait":
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = b.timeout
		}
		actionErr = b.WaitFor(ctx, step.Condition, timeout)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "wait matched " + step.Condition, "condition": step.Condition}
		}
	case "read":
		var read readability.PageRead
		read, actionErr = b.Read(ctx)
		sr.Result = read
		if actionErr == nil {
			sr.Message = "read captured"
		}
	case "snapshot":
		snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
		if err != nil {
			actionErr = err
			break
		}
		sr.Snapshot = &snap
		sr.Result = snap
		sr.Message = "snapshot captured"
	case "open":
		if step.URL == "" {
			actionErr = errors.New("open requires url")
			break
		}
		var openRes browser.OpenResult
		openRes, actionErr = b.Open(ctx, step.URL)
		if actionErr == nil {
			sr.Result = openRes
			actionErr = openRes.NavigationErr()
		}
		if actionErr == nil {
			retargetTo = openRes.Tab.ID
		}
	case "navigate_to":
		// Drives the existing tab; raw primitives so plans do not pay for an observed
		// wrapper on top of their own final observation.
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
	case "click_text":
		if step.Text == "" {
			actionErr = errors.New("click_text requires text")
			break
		}
		var label string
		label, actionErr = b.clickTextRaw(ctx, snapshot.ClickTextOptions{Text: step.Text})
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "clicked text " + strconv.Quote(label)}
		}
		b.settle(ctx, batchActionSettle)
	case "focus_tab":
		if step.ID == "" {
			actionErr = errors.New("focus_tab requires id")
			break
		}
		actionErr = b.FocusTab(ctx, step.ID)
		if actionErr == nil {
			retargetTo = step.ID
			sr.Result = map[string]any{"ok": true, "message": "focused tab " + step.ID, "tab_id": step.ID}
		}
	default:
		actionErr = fmt.Errorf("unknown action %q", step.Action)
	}

	if actionErr != nil {
		sr.OK = false
		sr.Error = actionErr.Error()
	}
	if sr.Message == "" && sr.OK {
		sr.Message = step.Action + " ok"
	}
	return sr, retargetTo
}

// waitChunkLimit keeps one in-page wait inside the bridge request timeout, with
// headroom for the WS round-trip.
func (b *Bridge) waitChunkLimit() time.Duration {
	limit := waitConditionChunk
	if budget := b.timeout - 2*time.Second; budget > 0 && budget < limit {
		limit = budget
	}
	if limit <= 0 {
		limit = time.Second
	}
	return limit
}

// waitConditionOnce awaits WaitConditionScript once: true when the condition
// holds, false at chunk. The check runs in the renderer on DOM mutations, so N
// concurrent waits cost N held evaluates, not N polling loops.
func (b *Bridge) waitConditionOnce(ctx context.Context, condition string, chunk time.Duration) (bool, error) {
	chunkMs := chunk.Milliseconds()
	if chunkMs < 0 {
		chunkMs = 0
	}
	condJSON, _ := json.Marshal(condition)
	expr := fmt.Sprintf("%s(%s,%d)", snapshot.WaitConditionScript, condJSON, chunkMs)
	var matched bool
	if err := b.evaluate(ctx, expr, "", &matched); err != nil {
		return false, err
	}
	return matched, nil
}
