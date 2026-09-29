package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/actions"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

const (
	// observedActionSettle and batchActionSettle cap the adaptive settle.
	observedActionSettle = 75 * time.Millisecond
	menuHoverSettleDelay = 325 * time.Millisecond
	batchActionSettle    = 25 * time.Millisecond
	waitForPollInterval  = 250 * time.Millisecond
	// Adaptive settle poll cadence: start tight, back off to settlePollMax.
	settlePollStart   = 12 * time.Millisecond
	settlePollMax     = 40 * time.Millisecond
	settleStableReads = 2
	// settleMinFloor keeps a setTimeout(0), render or rAF just after the action
	// observable even when the page already looks stable.
	settleMinFloor = 24 * time.Millisecond
	// waitConditionChunk resolves one in-page wait before b.timeout would cancel
	// it; WaitFor re-arms. waitForErrBackoff paces re-arming after a navigation.
	waitConditionChunk = 6 * time.Second
	waitForErrBackoff  = 100 * time.Millisecond
	// Bound contextTabID's retries through an MV3 reconnect before using the cache.
	activeTabResolveAttempts = 3
	activeTabResolveBackoff  = 150 * time.Millisecond
	// File-chooser upload wait for Page.fileChooserOpened.
	fileChooserPollTimeout  = 5 * time.Second
	fileChooserPollInterval = 200 * time.Millisecond
	// bridgeWriteTimeout is independent of the request ctx: coder/websocket closes
	// the WHOLE socket when a write's ctx is cancelled, so one cancelled request
	// queued behind a busy extension would drain every in-flight RPC.
	bridgeWriteTimeout = 10 * time.Second
)

// settleFingerprintExpr is a cheap O(1)-ish "has the page changed?" probe.
const settleFingerprintExpr = `(function(){try{
  var ae=document.activeElement;
  return document.readyState+'|'+(document.getElementsByTagName('*').length)+'|'+((document.body&&document.body.innerText)?document.body.innerText.length:0)+'|'+(ae?ae.tagName+'#'+(ae.id||''):'')+'|'+location.href;
}catch(e){return 'err';}})()`

// settle polls settleFingerprintExpr and returns once the page is stable and
// ready, or when capDur elapses. An unreadable page just waits out the cap.
func (b *Bridge) settle(ctx context.Context, capDur time.Duration) {
	if capDur <= 0 {
		return
	}
	start := time.Now()
	deadline := start.Add(capDur)
	floor := settleMinFloor
	if floor > capDur {
		floor = capDur
	}
	floorTime := start.Add(floor)
	interval := settlePollStart
	if interval > capDur {
		interval = capDur
	}
	prev := ""
	stable := 0
	// Bound the wait with a watchdog, not a short ctx: cancelling mid-write makes
	// coder/websocket drop the whole connection. An abandoned read finishes under b.timeout.
	read := func() (string, bool) {
		type fpRes struct {
			fp string
			ok bool
		}
		resCh := make(chan fpRes, 1)
		go func() {
			var fp string
			// withoutTabLock: an abandoned probe must not hold the tab lock and stall the
			// next action on that tab.
			err := b.evaluate(withoutTabLock(ctx), settleFingerprintExpr, "", &fp)
			resCh <- fpRes{fp: fp, ok: err == nil && fp != "" && fp != "err"}
		}()
		select {
		case r := <-resCh:
			return r.fp, r.ok
		case <-ctx.Done():
			return "", false
		case <-time.After(time.Until(deadline)):
			return "", false
		}
	}
	if fp, ok := read(); ok {
		prev = fp
		stable = 1
	}
	for time.Now().Before(deadline) {
		wait := interval
		if remaining := time.Until(deadline); wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		fp, ok := read()
		if !ok {
			continue
		}
		if fp == prev {
			stable++
			if stable >= settleStableReads && time.Now().After(floorTime) && (strings.HasPrefix(fp, "complete") || strings.HasPrefix(fp, "interactive")) {
				return
			}
		} else {
			prev = fp
			stable = 1
		}
		if interval < settlePollMax {
			interval += interval / 2 // mild geometric backoff (12,18,27,40…)
			if interval > settlePollMax {
				interval = settlePollMax
			}
		}
	}
}

func (b *Bridge) Click(ctx context.Context, ref string) (browser.ActionResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return browser.ActionResult{}, err
	}
	if err := browser.GuardCrossOriginRefs("click", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	before.Trace = b.traceOperands(before, browser.TraceEntry{Action: "click", Ref: ref})
	beforeTabs := b.captureTabIDs(ctx)
	if err := b.clickRef(ctx, ref); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBeforeAndTabs(ctx, "clicked "+ref, before, beforeTabs), nil
}

func (b *Bridge) ClickText(ctx context.Context, opts snapshot.ClickTextOptions) (browser.ActionResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	before.Trace = browser.TraceEntry{Action: "click_text", Text: opts.Text}
	beforeTabs := b.captureTabIDs(ctx)
	label, err := b.clickTextRaw(ctx, opts)
	if err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBeforeAndTabs(ctx, "clicked text "+strconv.Quote(label), before, beforeTabs), nil
}

// clickTextRaw clicks without pre/post observations, for batch and plan.
func (b *Bridge) clickTextRaw(ctx context.Context, opts snapshot.ClickTextOptions) (string, error) {
	optsJSON, _ := json.Marshal(opts)
	var clicked snapshot.ClickXYResult
	// User gesture on the FIRST evaluation too: an addEventListener handler is
	// invisible to the deferral check, so its window.open would be dropped.
	if err := b.evaluateWithUserGesture(ctx, fmt.Sprintf("%s(%s)", snapshot.ClickTextScript, optsJSON), "", &clicked); err != nil {
		return "", err
	}
	if !clicked.OK {
		if clicked.Error == "" {
			clicked.Error = "click text failed"
		}
		return "", fmt.Errorf("click text: %s", clicked.Error)
	}
	if clicked.Deferred {
		// The control needs a real gesture, so the script dispatched nothing. Retry
		// under Runtime.evaluate's userGesture; real CDP input is the fallback.
		if err := b.clickTextTrusted(ctx, opts, clicked); err != nil {
			return "", err
		}
	}
	label := opts.Text
	if clicked.Name != "" {
		label = clicked.Name
	}
	return label, nil
}

func (b *Bridge) Hover(ctx context.Context, ref string) (browser.ActionResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return browser.ActionResult{}, err
	}
	if err := browser.GuardCrossOriginRefs("hover", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	before.Trace = b.traceOperands(before, browser.TraceEntry{Action: "hover", Ref: ref})
	if err := b.hoverRef(ctx, ref); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "hovered "+ref, before), nil
}

func (b *Bridge) hoverRef(ctx context.Context, ref string) error {
	box, err := b.resolveBox(ctx, ref)
	if err != nil {
		return err
	}
	// JS hover listeners in one evaluate, then extension-applied CSS :hover. Only a
	// foreground tab gets trusted CDP pointer input: background tabs stall for
	// seconds on the Input ACK. Old extensions fall back to the blocking CDP command.
	refJSON, _ := json.Marshal(box.Ref)
	var hovered struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.HoverElementScript, refJSON), "", &hovered); err != nil {
		return err
	}
	if !hovered.OK {
		if hovered.Error == "" {
			hovered.Error = "hover event dispatch failed"
		}
		return errors.New(hovered.Error)
	}
	_, err = b.call(ctx, "move_pointer", map[string]any{
		"tabId": parseTabID(b.contextTabID(ctx)),
		"x":     box.ViewportX,
		"y":     box.ViewportY,
	})
	if err != nil && isUnknownMessageTypeErr(err) {
		_, err = b.cdp(ctx, "", "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseMoved",
			"x":    box.ViewportX,
			"y":    box.ViewportY,
		})
	}
	if err != nil {
		return err
	}
	if box.DelayedHover {
		timer := time.NewTimer(menuHoverSettleDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func (b *Bridge) clickRef(ctx context.Context, ref string) error {
	box, err := b.resolveBox(ctx, ref)
	if err != nil {
		if fallbackErr := b.activate(ctx, ref); fallbackErr == nil {
			return nil
		}
		return err
	}
	// In-page click first: CDP Input.dispatchMouseEvent waits on a renderer ack
	// that costs ~1.5s per event on heavy pages. CDP stays the fallback when the
	// point is not hit-testable in-page.
	xJSON, _ := json.Marshal(box.ViewportX)
	yJSON, _ := json.Marshal(box.ViewportY)
	var inPage snapshot.ClickXYResult
	expression := fmt.Sprintf("%s(%s,%s)", snapshot.ClickXYScript, xJSON, yJSON)
	// userGesture on EVERY click: whether a listener is gesture-gated cannot be
	// read back from page script, and guessing wrong drops window.open.
	evalErr := b.evaluateWithUserGesture(ctx, expression, "", &inPage)
	if evalErr == nil && inPage.OK {
		return nil
	}
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		if _, err := b.cdp(ctx, "", "Input.dispatchMouseEvent", map[string]any{
			"type":   typ,
			"x":      box.ViewportX,
			"y":      box.ViewportY,
			"button": "left",
			"buttons": func() int {
				if typ == "mousePressed" {
					return 1
				}
				return 0
			}(),
			"clickCount": 1,
		}); err != nil {
			return err
		}
	}
	return nil
}

// clickTextTrusted clicks a click_text target that needs a genuine input gesture.
func (b *Bridge) clickTextTrusted(ctx context.Context, opts snapshot.ClickTextOptions, deferred snapshot.ClickXYResult) error {
	retry := opts
	retry.NoDefer = true
	retryJSON, _ := json.Marshal(retry)
	var clicked snapshot.ClickXYResult
	err := b.evaluateWithUserGesture(ctx, fmt.Sprintf("%s(%s)", snapshot.ClickTextScript, retryJSON), "", &clicked)
	if err == nil && clicked.OK && !clicked.Deferred {
		return nil
	}
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		buttons := 0
		if typ == "mousePressed" {
			buttons = 1
		}
		if _, dispatchErr := b.cdp(ctx, "", "Input.dispatchMouseEvent", map[string]any{
			"type":       typ,
			"x":          deferred.X,
			"y":          deferred.Y,
			"button":     "left",
			"buttons":    buttons,
			"clickCount": 1,
		}); dispatchErr != nil {
			return dispatchErr
		}
	}
	return nil
}

func (b *Bridge) activate(ctx context.Context, ref string) error {
	refJSON, _ := json.Marshal(ref)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	// Uses the shared __abFindDeep lookup so same-origin iframe and shadow refs resolve.
	expr := fmt.Sprintf(`(function(ref) {`+snapshot.FrameWalkHelpers+`
	  function findByRef(ref) {
	    var hit = __abFindDeep(ref);
	    return hit ? hit.el : null;
	  }
	  const el = findByRef(ref);
	  if (!el) return { ok: false, error: 'ref not found' };
	  if (el.closest('[hidden],[aria-hidden="true"]')) return { ok: false, error: 'ref hidden' };
	  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
	  if (typeof el.focus === 'function') el.focus({ preventScroll: true });
	  el.dispatchEvent(new MouseEvent('mouseover', { bubbles: true, cancelable: true, view: window }));
	  el.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true, view: window }));
	  el.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, cancelable: true, view: window }));
	  if (typeof el.click === 'function') el.click();
	  else el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, view: window }));
	  return { ok: true };
	})(%s)`, refJSON)
	if err := b.evaluate(ctx, expr, "", &result); err != nil {
		return err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "ref activation failed"
		}
		return fmt.Errorf("activate: %s", result.Error)
	}
	return nil
}

func (b *Bridge) Type(ctx context.Context, ref, text string) (browser.ActionResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return browser.ActionResult{}, err
	}
	if err := browser.GuardCrossOriginRefs("type", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	before.Trace = b.traceOperands(before, browser.RedactTraceEntry(ctx, browser.TraceEntry{Action: "type", Ref: ref, Text: text}))
	if err := b.typeRef(ctx, ref, text); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "typed into "+ref, before), nil
}

func (b *Bridge) typeRef(ctx context.Context, ref, text string) error {
	if err := b.focus(ctx, ref); err != nil {
		return err
	}
	return b.pacer.Type(ctx, text, func(chunk string) error {
		_, err := b.cdp(ctx, "", "Input.insertText", map[string]any{"text": chunk})
		return err
	})
}

// Focus focuses one element and reports the page afterwards.
func (b *Bridge) Focus(ctx context.Context, ref string) (browser.ActionResult, error) {
	if err := browser.GuardCrossOriginRefs("focus", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return browser.ActionResult{}, err
	}
	if strings.TrimSpace(ref) == "" {
		return browser.ActionResult{}, errors.New("ref is required")
	}
	before := b.captureSemanticState(ctx)
	before.Trace = b.traceOperands(before, browser.RedactTraceEntry(ctx, browser.TraceEntry{Action: "focus", Ref: ref}))
	if err := b.focus(ctx, ref); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "focused "+ref, before), nil
}

// FocusRef focuses an element for deterministic recipe key presses.
func (b *Bridge) FocusRef(ctx context.Context, ref string) error {
	if err := browser.GuardCrossOriginRefs("focus", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return err
	}
	return b.focus(ctx, ref)
}

func (b *Bridge) Fill(ctx context.Context, opts snapshot.FillOptions) (browser.ActionResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return browser.ActionResult{}, err
	}
	if err := browser.GuardCrossOriginRefs("fill", browser.BridgeCrossOriginRemedy, opts.Ref); err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	before.Trace = b.traceOperands(before, browser.RedactTraceEntry(ctx, browser.TraceEntry{Action: "fill", Ref: opts.Ref, Text: opts.EffectiveText()}))
	ref, err := b.fillOptions(ctx, opts)
	if err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "filled "+ref, before), nil
}

func (b *Bridge) fillOptions(ctx context.Context, opts snapshot.FillOptions) (string, error) {
	ref, err := b.resolveFillRef(ctx, opts)
	if err != nil {
		return "", err
	}
	refJSON, _ := json.Marshal(ref)
	textJSON, _ := json.Marshal(opts.EffectiveText())
	replaceJSON, _ := json.Marshal(opts.Replace)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s,%s,%s)", snapshot.FillElementScript, refJSON, textJSON, replaceJSON), "", &result); err != nil {
		return "", err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "fill failed"
		}
		return "", fmt.Errorf("fill: %s", result.Error)
	}
	return ref, nil
}

func (b *Bridge) resolveFillRef(ctx context.Context, opts snapshot.FillOptions) (string, error) {
	if opts.Ref != "" {
		return opts.Ref, nil
	}
	result, err := b.Find(ctx, snapshot.FindOptions{
		Query: opts.Query,
		Role:  opts.Role,
		Limit: 1,
	})
	if err != nil {
		return "", err
	}
	if len(result.Elements) == 0 {
		return "", fmt.Errorf("no fill target found for query %q", opts.Query)
	}
	return result.Elements[0].Ref, nil
}

func (b *Bridge) UploadFile(ctx context.Context, opts snapshot.UploadOptions) (browser.ActionResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return browser.ActionResult{}, err
	}
	if err := browser.GuardCrossOriginRefs("upload file", browser.BridgeCrossOriginRemedy, opts.Ref, opts.ClickRef); err != nil {
		return browser.ActionResult{}, err
	}
	// bytes/url sources become temp files, retained briefly after
	// DOM.setFileInputFiles so a later form submit can still read them.
	paths, cleanup, err := browser.ResolveUploadPaths(ctx, opts)
	if err != nil {
		return browser.ActionResult{}, err
	}
	cleanupNow := true
	defer func() {
		if cleanupNow {
			cleanup()
		}
	}()

	// With a trigger named, intercept the native chooser and set the file on the
	// input it reports. Covers inputs created on click (a native dialog would
	// freeze the CDP session) and cross-origin iframes (backendNodeId is frame-agnostic).
	if opts.ClickRef != "" || opts.ClickText != "" {
		result, err := b.uploadViaFileChooser(ctx, opts, paths)
		if err != nil {
			return browser.ActionResult{}, err
		}
		browser.RetainUploadCleanup(cleanup)
		cleanupNow = false
		return result, nil
	}

	ref := opts.Ref
	if ref == "" {
		query := opts.Query
		if strings.TrimSpace(query) == "" {
			query = "file"
		}
		result, err := b.Find(ctx, snapshot.FindOptions{
			Query: query,
			Role:  opts.Role,
			Limit: 20,
		})
		if err != nil {
			return browser.ActionResult{}, err
		}
		for _, el := range result.Elements {
			if el.Tag == "input" && el.Type == "file" {
				ref = el.Ref
				break
			}
		}
		if ref == "" {
			return browser.ActionResult{}, fmt.Errorf("no file input found for query %q", query)
		}
	}

	refJSON, _ := json.Marshal(ref)
	raw, err := b.cdp(ctx, "", "Runtime.evaluate", map[string]any{
		"expression":    fmt.Sprintf("%s(%s)", snapshot.FileInputElementScript, refJSON),
		"returnByValue": false,
		"awaitPromise":  true,
		"objectGroup":   "brw-upload",
	})
	if err != nil {
		return browser.ActionResult{}, err
	}
	var eval struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
		ExceptionDetails any `json:"exceptionDetails,omitempty"`
	}
	if err := json.Unmarshal(raw, &eval); err != nil {
		return browser.ActionResult{}, err
	}
	if eval.ExceptionDetails != nil {
		details, _ := json.Marshal(eval.ExceptionDetails)
		return browser.ActionResult{}, fmt.Errorf("file input resolution failed: %s", details)
	}
	if eval.Result.ObjectID == "" {
		return browser.ActionResult{}, errors.New("file input resolution returned no object id")
	}
	defer func() {
		_, _ = b.cdp(ctx, "", "Runtime.releaseObject", map[string]any{"objectId": eval.Result.ObjectID})
	}()
	before := b.captureSemanticState(ctx)
	if _, err := b.cdp(ctx, "", "DOM.setFileInputFiles", map[string]any{
		"files":    paths,
		"objectId": eval.Result.ObjectID,
	}); err != nil {
		return browser.ActionResult{}, err
	}
	var ignored any
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.FileInputEventsScript, refJSON), "", &ignored); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	result := b.observeActionWithBefore(ctx, "uploaded file to "+ref, before)
	browser.RetainUploadCleanup(cleanup)
	cleanupNow = false
	return result, nil
}

// uploadViaFileChooser clicks the trigger with native-dialog interception on
// and sets the file on the chooser's backendNodeId. Interception is always
// disabled on exit so the user's own uploads work.
func (b *Bridge) uploadViaFileChooser(ctx context.Context, opts snapshot.UploadOptions, paths []string) (browser.ActionResult, error) {
	// Pin the tab so every step hits the same tab if the user switches mid-upload.
	tabID := b.contextTabID(ctx)

	if _, err := b.call(ctx, "set_intercept_file_chooser", map[string]any{
		"tabId":   parseTabID(tabID),
		"enabled": true,
	}); err != nil {
		return browser.ActionResult{}, fmt.Errorf("enable file chooser interception: %w", err)
	}
	defer func() {
		// Fresh context so a cancelled ctx cannot leave interception on.
		disableCtx, cancel := context.WithTimeout(context.Background(), b.timeout)
		defer cancel()
		_, _ = b.call(disableCtx, "set_intercept_file_chooser", map[string]any{
			"tabId":   parseTabID(tabID),
			"enabled": false,
		})
	}()

	before := b.captureSemanticState(ctx)

	if opts.ClickRef != "" {
		if err := b.clickRef(ctx, opts.ClickRef); err != nil {
			return browser.ActionResult{}, fmt.Errorf("click upload trigger %s: %w", opts.ClickRef, err)
		}
	} else {
		optsJSON, _ := json.Marshal(snapshot.ClickTextOptions{Text: opts.ClickText, Role: opts.Role})
		var clicked snapshot.ClickXYResult
		if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.ClickTextScript, optsJSON), tabID, &clicked); err != nil {
			return browser.ActionResult{}, fmt.Errorf("click upload trigger %q: %w", opts.ClickText, err)
		}
		if !clicked.OK {
			msg := clicked.Error
			if msg == "" {
				msg = "click text failed"
			}
			return browser.ActionResult{}, fmt.Errorf("click upload trigger %q: %s", opts.ClickText, msg)
		}
	}

	var backendNodeID int64
	deadline := time.Now().Add(fileChooserPollTimeout)
	for {
		var ev struct {
			Captured      bool  `json:"captured"`
			BackendNodeID int64 `json:"backendNodeId"`
		}
		raw, err := b.call(ctx, "get_file_chooser_event", map[string]any{"tabId": parseTabID(tabID)})
		if err == nil {
			if jsonErr := json.Unmarshal(raw, &ev); jsonErr == nil && ev.Captured {
				if ev.BackendNodeID == 0 {
					return browser.ActionResult{}, errors.New("file chooser opened but reported no backendNodeId")
				}
				backendNodeID = ev.BackendNodeID
				break
			}
		}
		if time.Now().After(deadline) {
			return browser.ActionResult{}, fmt.Errorf("no file chooser opened within %s after clicking the trigger — confirm the trigger opens a file picker", fileChooserPollTimeout)
		}
		select {
		case <-ctx.Done():
			return browser.ActionResult{}, ctx.Err()
		case <-time.After(fileChooserPollInterval):
		}
	}

	if _, err := b.cdp(ctx, tabID, "DOM.setFileInputFiles", map[string]any{
		"files":         paths,
		"backendNodeId": backendNodeID,
	}); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "uploaded file via intercepted file chooser", before), nil
}

func (b *Bridge) Select(ctx context.Context, ref, value string) (browser.ActionResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return browser.ActionResult{}, err
	}
	if err := browser.GuardCrossOriginRefs("select", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	before.Trace = b.traceOperands(before, browser.RedactTraceEntry(ctx, browser.TraceEntry{Action: "select", Ref: ref, Value: value}))
	message, err := b.selectValue(ctx, ref, value)
	if err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, message, before), nil
}

func (b *Bridge) selectValue(ctx context.Context, ref, value string) (string, error) {
	refJSON, _ := json.Marshal(ref)
	valueJSON, _ := json.Marshal(value)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s,%s)", snapshot.SelectElementScript, refJSON, valueJSON), "", &result); err != nil {
		return "", err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "select failed"
		}
		if !strings.Contains(result.Error, snapshot.NotASelectElement) {
			return "", fmt.Errorf("select: %s", result.Error)
		}
		return b.selectCustomOption(ctx, ref, value)
	}
	return "selected " + ref, nil
}

func (b *Bridge) selectCustomOption(ctx context.Context, ref, value string) (string, error) {
	if b.elementValueMatches(ctx, ref, value) {
		return "selected " + ref + " already " + value, nil
	}
	option, err := b.findOptionCandidate(ctx, value)
	if err != nil {
		if err := b.clickRef(ctx, ref); err != nil {
			return "", fmt.Errorf("open custom select %s: %w", ref, err)
		}
		b.settle(ctx, observedActionSettle)
		option, err = b.findOptionCandidate(ctx, value)
		if err != nil {
			return "", err
		}
	}
	if err := b.clickRef(ctx, option.Ref); err != nil {
		return "", fmt.Errorf("select option %s: %w", option.Ref, err)
	}
	return "selected " + ref + " via option " + option.Ref, nil
}

func (b *Bridge) elementValueMatches(ctx context.Context, ref, value string) bool {
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{Limit: 0, ViewportOnly: false})
	if err != nil {
		return false
	}
	for _, el := range snap.Elements {
		if el.Ref == ref && browser.ElementMatchesOptionValue(el, value) {
			return true
		}
	}
	return false
}

func (b *Bridge) findOptionCandidate(ctx context.Context, value string) (snapshot.Element, error) {
	for _, opts := range []snapshot.SnapshotOptions{
		{Role: "option", Query: value, Limit: 100, ViewportOnly: false},
		{Role: "option", Limit: 200, ViewportOnly: false},
	} {
		snap, err := b.Snapshot(ctx, opts)
		if err != nil {
			return snapshot.Element{}, err
		}
		if option, ok := browser.SelectOptionCandidate(snap.Elements, value); ok {
			return option, nil
		}
	}
	return snapshot.Element{}, fmt.Errorf("no visible option found for %q", value)
}

func (b *Bridge) Press(ctx context.Context, key string) (browser.ActionResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	before.Trace = browser.TraceEntry{Action: "press", Value: key}
	if err := b.pressKey(ctx, key); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "pressed "+key, before), nil
}

func (b *Bridge) pressKey(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("key is required")
	}
	desc := actions.DescribeKey(key)
	if desc.Key == "" {
		return errors.New("key is required")
	}
	// Chrome silently drops Input.dispatchKeyEvent for an inactive tab, and
	// activating it would flash-switch the user's tab; use the DOM fallback there.
	rawState, stateErr := b.call(ctx, "get_tab_input_state", map[string]any{
		"tabId": parseTabID(b.contextTabID(ctx)),
	})
	if stateErr == nil {
		var state struct {
			Active bool `json:"active"`
		}
		if err := json.Unmarshal(rawState, &state); err != nil {
			return err
		}
		if !state.Active {
			descJSON, _ := json.Marshal(map[string]any{
				"key":       desc.Key,
				"code":      desc.Code,
				"text":      desc.Text,
				"keyCode":   desc.WindowsVirtualKeyCode,
				"modifiers": desc.Modifiers,
			})
			var result struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.PressKeyFallbackScript, descJSON), "", &result); err != nil {
				return err
			}
			if !result.OK {
				if result.Error == "" {
					result.Error = "background key fallback failed"
				}
				return errors.New(result.Error)
			}
			return nil
		}
	} else if !isUnknownMessageTypeErr(stateErr) {
		return stateErr
	}
	for _, typ := range []string{"keyDown", "keyUp"} {
		if typ == "keyDown" && desc.Text == "" {
			// rawKeyDown triggers Chrome's native non-text defaults (ArrowUp on number inputs).
			typ = "rawKeyDown"
		}
		params := map[string]any{
			"type":                  typ,
			"modifiers":             desc.Modifiers,
			"key":                   desc.Key,
			"code":                  desc.Code,
			"windowsVirtualKeyCode": desc.WindowsVirtualKeyCode,
			"nativeVirtualKeyCode":  desc.WindowsVirtualKeyCode,
		}
		if typ == "keyDown" && desc.Text != "" {
			params["text"] = desc.Text
			params["unmodifiedText"] = desc.Text
		}
		if _, err := b.cdp(ctx, "", "Input.dispatchKeyEvent", params); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bridge) Scroll(ctx context.Context, direction string) (browser.ActionResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	before.Trace = browser.TraceEntry{Action: "scroll", Value: direction}
	message, err := b.scrollDirection(ctx, direction)
	if err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, message, before), nil
}

func (b *Bridge) scrollDirection(ctx context.Context, direction string) (string, error) {
	direction = strings.ToLower(strings.TrimSpace(direction))
	if direction == "" {
		direction = "down"
	}
	directionJSON, _ := json.Marshal(direction)
	var scroll snapshot.ScrollResult
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.ScrollPageScript, directionJSON), "", &scroll); err != nil {
		return "", err
	}
	if !scroll.OK {
		if scroll.Error == "" {
			scroll.Error = "scroll failed"
		}
		return "", fmt.Errorf("scroll: %s", scroll.Error)
	}
	message := fmt.Sprintf("scrolled %s target:%s", direction, scroll.Target)
	if scroll.Name != "" {
		message += " " + strconv.Quote(scroll.Name)
	}
	return message, nil
}

func (b *Bridge) resolveBox(ctx context.Context, ref string) (snapshot.ElementBox, error) {
	refJSON, _ := json.Marshal(ref)
	var box snapshot.RecoveredBox
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.ResolveOrRecoverBoxScript, refJSON), "", &box); err != nil {
		return snapshot.ElementBox{}, err
	}
	if !box.OK {
		reason := box.Reason
		if reason == "" {
			reason = "not_visible"
		}
		return snapshot.ElementBox{}, fmt.Errorf("element ref %q not recoverable: %s", ref, reason)
	}
	return box.ElementBox, nil
}

func (b *Bridge) focus(ctx context.Context, ref string) error {
	refJSON, _ := json.Marshal(ref)
	var ok bool
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.FocusElementScript, refJSON), "", &ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("element ref %q not found or could not be focused", ref)
	}
	return nil
}

func (b *Bridge) ClickXY(ctx context.Context, x, y float64) (snapshot.ClickXYResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return snapshot.ClickXYResult{}, err
	}
	var result snapshot.ClickXYResult
	xJSON, _ := json.Marshal(x)
	yJSON, _ := json.Marshal(y)
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s,%s)", snapshot.ClickXYScript, xJSON, yJSON), "", &result); err != nil {
		return snapshot.ClickXYResult{}, err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "click failed"
		}
		return result, fmt.Errorf("click xy: %s", result.Error)
	}
	return result, nil
}
