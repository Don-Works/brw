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
	// observedActionSettle / batchActionSettle are the CAPS for the adaptive
	// settle (b.settle): the page-change poll never blocks longer than this, so
	// settling is never slower than the previous blind time.Sleep, only faster
	// when the page stabilises early.
	observedActionSettle = 75 * time.Millisecond
	menuHoverSettleDelay = 325 * time.Millisecond
	batchActionSettle    = 25 * time.Millisecond
	waitForPollInterval  = 250 * time.Millisecond
	// settlePollStart / settlePollMax bound the adaptive settle poll cadence:
	// start tight so a quiescent page returns in ~one short interval, then back
	// off so a busy page does not spin. settleStableReads is how many consecutive
	// equal fingerprints count as "settled".
	settlePollStart   = 12 * time.Millisecond
	settlePollMax     = 40 * time.Millisecond
	settleStableReads = 2
	// settleMinFloor is the minimum settle duration: we never return "settled"
	// before it even when the page already looks stable, so a handler's
	// setTimeout(0) / framework render / rAF that lands a few ms after the action
	// is still observed (preserving the debounce the old fixed sleep gave). Capped
	// by capDur, so a batch step's 25ms cap is honoured.
	settleMinFloor = 24 * time.Millisecond
	// waitConditionChunk bounds a single in-page wait-promise await so one held
	// Runtime.evaluate resolves (returning false at the chunk timeout) before the
	// bridge request timeout (b.timeout) would cancel it; WaitFor re-arms the promise
	// until its own deadline. waitForErrBackoff paces re-arming after a navigation
	// destroys the in-page execution context mid-await, so WaitFor never hot-loops.
	waitConditionChunk = 6 * time.Second
	waitForErrBackoff  = 100 * time.Millisecond
	// activeTabResolveAttempts/Backoff bound how hard contextTabID retries live
	// active-tab resolution before falling back to the last-known cached tab. The
	// MV3 service worker can be mid-reconnect when a call lands; a couple of quick
	// retries ride that out so we don't act on a stale tab.
	activeTabResolveAttempts = 3
	activeTabResolveBackoff  = 150 * time.Millisecond
	// fileChooserPollTimeout/Interval bound how long file-chooser-interception
	// upload mode waits for the Page.fileChooserOpened event after clicking the
	// trigger before giving up, and how often it polls the extension for it.
	fileChooserPollTimeout  = 5 * time.Second
	fileChooserPollInterval = 200 * time.Millisecond
	// bridgeWriteTimeout bounds a single WS frame write with a deadline INDEPENDENT of
	// the request context. coder/websocket registers context.AfterFunc(writeCtx, close)
	// for the duration of a write, so binding the write to the request ctx means a
	// request that cancels (a long wait hitting b.timeout, the upstream 20s HTTP cap, or
	// a caller giving up) while a frame is queued behind a busy extension tears down the
	// WHOLE shared socket — every in-flight RPC then drains "extension disconnected" and
	// the extension reconnects (~1s). That is the observed "10 concurrent heavy calls
	// wedge the bridge, then it auto-recovers". A write completes in well under this cap;
	// decoupling it stops one slow/cancelled request from wedging every concurrent call.
	bridgeWriteTimeout = 10 * time.Second
)

// settleFingerprintExpr is a cheap in-page snapshot of "has the page changed?"
// signals: readyState, the DOM node count, the body text length, the active
// element tag, and the current URL. It is intentionally O(1)-ish (no full DOM
// serialization) so polling it a few times is far cheaper than a real snapshot.
const settleFingerprintExpr = `(function(){try{
  var ae=document.activeElement;
  return document.readyState+'|'+(document.getElementsByTagName('*').length)+'|'+((document.body&&document.body.innerText)?document.body.innerText.length:0)+'|'+(ae?ae.tagName+'#'+(ae.id||''):'')+'|'+location.href;
}catch(e){return 'err';}})()`

// settle replaces the previous blind time.Sleep(capDur) before observing an
// action. It polls a cheap in-page fingerprint and returns as soon as the page
// is stable (two consecutive equal reads) and ready, or when capDur elapses — so
// it is NEVER slower than the old fixed sleep, only faster on a quiescent page.
// It is cancellation-aware (returns immediately if ctx is done) and degrades to
// honouring the remaining cap if the fingerprint cannot be read (disconnected /
// mid-navigation).
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
	// Each fingerprint read is bounded by the settle deadline (not b.timeout), so a
	// connected-but-unresponsive extension cannot turn a 25/75ms settle into a
	// multi-second b.call wait. We bound the WAIT with a watchdog rather than
	// passing a cap-short context to b.evaluate: a context cancelled mid-write
	// makes coder/websocket drop the whole connection, so a slow-but-healthy write
	// must not be force-cancelled. An abandoned read completes harmlessly in the
	// background via the normal b.timeout.
	read := func() (string, bool) {
		type fpRes struct {
			fp string
			ok bool
		}
		resCh := make(chan fpRes, 1)
		go func() {
			var fp string
			// withoutTabLock: this probe can be abandoned by the watchdog below
			// while it keeps running, so it must not hold the tab's serialization
			// lock and stall the next foreground action on that tab.
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
			// Cannot read the page (navigating / disconnected): keep honouring
			// the remaining cap as a plain wait, matching the old behaviour.
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

// clickTextRaw performs the semantic click without pre/post observations. Batch
// and plan callers use it so they retain their one-final-observation contract.
func (b *Bridge) clickTextRaw(ctx context.Context, opts snapshot.ClickTextOptions) (string, error) {
	optsJSON, _ := json.Marshal(opts)
	var clicked snapshot.ClickXYResult
	// User gesture on the FIRST evaluation, not only on the deferred retry: a
	// gesture-gated handler registered with addEventListener is invisible to the
	// deferral check, so it never reached the retry and its window.open was
	// dropped under a click that reported success.
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
		// The resolved control only responds to a real input gesture, so the
		// script deliberately did not dispatch anything. Re-run it under
		// Runtime.evaluate's userGesture flag, which grants the transient
		// activation window.open/target=_blank/download/fullscreen require while
		// keeping the single in-page round trip. Real CDP input is the fallback
		// when that still does not take.
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
	// Fire JS hover listeners synchronously in one fast evaluate, then let the
	// extension apply deterministic CSS :hover to the hit-tested ancestor chain.
	// A genuinely foreground tab also gets trusted CDP pointer input; background
	// or locked tabs avoid the multi-second Input ACK that would block nested menu
	// work. Old extensions fall back to the blocking CDP command for compatibility.
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
	// Fast path: actuate the click with a single in-page round-trip. CDP
	// Input.dispatchMouseEvent blocks on a renderer ack that can cost ~1.5s per
	// call on heavy pages (≈5s for the three-event sequence below); the in-page
	// pointer/mouse/click sequence fires the same handlers in one Runtime.evaluate
	// (~tens of ms). Trusted CDP dispatch stays as the fallback when the point is
	// not hit-testable in-page (e.g. element scrolled out of the layout viewport).
	xJSON, _ := json.Marshal(box.ViewportX)
	yJSON, _ := json.Marshal(box.ViewportY)
	var inPage snapshot.ClickXYResult
	expression := fmt.Sprintf("%s(%s,%s)", snapshot.ClickXYScript, xJSON, yJSON)
	// Runtime.evaluate's userGesture flag grants the transient activation that
	// window.open/download/fullscreen controls require, while retaining the
	// one-round-trip in-page click path. It is applied to EVERY click, not only
	// the shapes resolveBox could recognise: a listener registered with
	// addEventListener cannot be read back from page script, so "this control is
	// gesture-gated" is not decidable in advance. Predicting it wrongly meant a
	// dropped window.open under a click that reported success.
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

// clickTextTrusted actuates a click_text target that needs a genuine input
// gesture, which the ordinary in-page dispatch cannot provide.
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
	// The lookup is the shared one (__abFindDeep), not a private copy. This used
	// to walk only the top document and its open shadow roots, so the click
	// fallback silently failed on refs the rest of brw resolves fine — anything in
	// a same-origin iframe — and it was the one ref path that did not name a
	// cross-origin ref for what it is.
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
	_, err := b.cdp(ctx, "", "Input.insertText", map[string]any{"text": text})
	return err
}

// Focus gives one element the keyboard focus and reports the page afterwards,
// matching the observation contract every action tool answers on.
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

// FocusRef supports deterministic recipe key presses without exposing another
// model-facing tool or relying on ambient focus from a previous step.
func (b *Bridge) FocusRef(ctx context.Context, ref string) error {
	if err := browser.GuardCrossOriginRefs("focus", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return err
	}
	return b.focus(ctx, ref)
}

func (b *Bridge) Fill(ctx context.Context, opts snapshot.FillOptions) (browser.ActionResult, error) {
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
	if err := browser.GuardCrossOriginRefs("upload file", browser.BridgeCrossOriginRemedy, opts.Ref, opts.ClickRef); err != nil {
		return browser.ActionResult{}, err
	}
	// Resolve the upload source (local path(s), inline bytes_base64, or remote
	// URL). bytes/url sources are materialized to temp files on the daemon host
	// and retained briefly after DOM.setFileInputFiles so a later form submission
	// can still read their contents.
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

	// File-chooser-interception mode: when a trigger is named, click it with the
	// native chooser intercepted and set the file on whatever input the chooser
	// reports. Handles SPAs that create the input on click (which would otherwise
	// freeze the CDP session behind a native OS dialog) and inputs in cross-origin
	// iframes (backendNodeId is frame-agnostic).
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

// uploadViaFileChooser drives the file-chooser-interception upload path: enable
// native-dialog interception, click the trigger, capture the chooser's
// backendNodeId from the Page.fileChooserOpened event the extension stashes, and
// set the file with DOM.setFileInputFiles. Interception is ALWAYS disabled on
// exit so the user's manual uploads in this Chrome are unaffected.
func (b *Bridge) uploadViaFileChooser(ctx context.Context, opts snapshot.UploadOptions, paths []string) (browser.ActionResult, error) {
	// Pin the tab for the whole sequence so interception, click, poll, and set all
	// target the same tab even if the user switches tabs mid-upload.
	tabID := b.contextTabID(ctx)

	if _, err := b.call(ctx, "set_intercept_file_chooser", map[string]any{
		"tabId":   parseTabID(tabID),
		"enabled": true,
	}); err != nil {
		return browser.ActionResult{}, fmt.Errorf("enable file chooser interception: %w", err)
	}
	defer func() {
		// Always restore manual uploads, even on error. Use a fresh context so a
		// cancelled/expired ctx cannot leave interception stuck on.
		disableCtx, cancel := context.WithTimeout(context.Background(), b.timeout)
		defer cancel()
		_, _ = b.call(disableCtx, "set_intercept_file_chooser", map[string]any{
			"tabId":   parseTabID(tabID),
			"enabled": false,
		})
	}()

	before := b.captureSemanticState(ctx)

	// Click the trigger that opens the (now intercepted) native chooser.
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

	// Poll for the captured Page.fileChooserOpened event (up to ~5s).
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
	// Chrome silently discards Input.dispatchKeyEvent for an inactive tab. Do not
	// activate it (which would flash-switch the user's current tab); use a bounded
	// standards-DOM fallback there. Foreground tabs keep the trusted CDP path.
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
			// rawKeyDown is required for Chrome's native non-text default actions
			// (ArrowUp on number inputs, navigation keys, and similar controls).
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
