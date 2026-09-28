package extensionbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func (b *Bridge) Evaluate(ctx context.Context, expression string) (any, error) {
	start := time.Now()
	// An expression can carry a value a sensitive recipe step supplied, so the
	// same redaction the input actions use applies before the text is recorded.
	// The action is still recorded; only the script goes.
	record := func(err error) {
		tabID := b.contextTabID(ctx)
		if strings.TrimSpace(tabID) == "" {
			return
		}
		action, text := browser.TraceActionEvaluate, expression
		if label, ok := browser.TraceLabelFromCtx(ctx); ok {
			action, text = label.Action, label.Value
		}
		entry := browser.RedactTraceEntry(ctx, browser.NewObservationTrace(action, text, start, err))
		entry.TabID = tabID
		b.appendTrace(entry)
	}
	var result json.RawMessage
	if err := b.evaluateUserExpression(ctx, expression, "", &result); err != nil {
		record(err)
		return nil, err
	}
	var value any
	if err := json.Unmarshal(result, &value); err != nil {
		record(err)
		return nil, err
	}
	record(nil)
	return value, nil
}

func (b *Bridge) evaluate(ctx context.Context, expression, tabID string, dst any) error {
	return b.evaluateRuntime(ctx, expression, tabID, false, false, dst)
}

func (b *Bridge) evaluateWithUserGesture(ctx context.Context, expression, tabID string, dst any) error {
	return b.evaluateRuntime(ctx, expression, tabID, true, false, dst)
}

func (b *Bridge) evaluateUserExpression(ctx context.Context, expression, tabID string, dst any) error {
	err := b.evaluate(ctx, expression, tabID, dst)
	if err == nil || !isTopLevelAwaitSyntaxError(err) {
		return err
	}
	// Runtime.evaluate's normal mode correctly awaits returned Promises, but raw
	// top-level await is a syntax error. Retry only that parse failure in REPL
	// mode; using REPL mode for every internal evaluation would turn an IIFE that
	// returns a Promise into an unserializable Promise object instead of awaiting
	// it (breaking wait_for and adaptive settle).
	return b.evaluateRuntime(ctx, expression, tabID, false, true, dst)
}

func isTopLevelAwaitSyntaxError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "await is only valid") ||
		strings.Contains(message, "unexpected reserved word") ||
		strings.Contains(message, "await is not defined")
}

// evaluateReadOnly retries a renderer-context rollover for idempotent reads.
// A freshly opened popup can commit between target discovery and Runtime.evaluate;
// surfacing that harmless race forces agents to add arbitrary sleeps. Mutating
// evaluations deliberately do not use this helper because replaying them could
// duplicate an action.
func (b *Bridge) evaluateReadOnly(ctx context.Context, expression, tabID string, dst any) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = b.evaluate(ctx, expression, tabID, dst)
		if err == nil || !isNavigationTeardownError(err) {
			return err
		}
		backoff := time.Duration(attempt+1) * 75 * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return err
}

func (b *Bridge) evaluateRuntime(ctx context.Context, expression, tabID string, userGesture, replMode bool, dst any) error {
	params := map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}
	if userGesture {
		params["userGesture"] = true
	}
	if replMode {
		params["replMode"] = true
	}
	raw, err := b.cdp(ctx, tabID, "Runtime.evaluate", params)
	if err != nil {
		return err
	}
	var payload struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails any `json:"exceptionDetails,omitempty"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.ExceptionDetails != nil {
		if msg := browser.FormatRuntimeException(payload.ExceptionDetails); msg != "" {
			return fmt.Errorf("runtime exception: %s", msg)
		}
		details, _ := json.Marshal(payload.ExceptionDetails)
		return fmt.Errorf("runtime exception: %s", details)
	}
	if len(payload.Result.Value) == 0 {
		// Void/undefined results (e.g. location.reload(), assignments, calls that
		// return nothing) are a successful evaluation, not an error. Surface them
		// as JSON null rather than failing the whole tool call.
		if err := json.Unmarshal([]byte("null"), dst); err != nil {
			return err
		}
	} else if err := json.Unmarshal(payload.Result.Value, dst); err != nil {
		return err
	}
	return b.guardCurrentURL(ctx)
}

func (b *Bridge) cdp(ctx context.Context, tabID, method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	req := map[string]any{"method": method, "params": params}
	if strings.TrimSpace(tabID) == "" {
		tabID = b.contextTabID(ctx)
	}
	if tabID != "" {
		req["tabId"] = parseTabID(tabID)
	}
	raw, err := b.call(ctx, "cdp", req)
	if err != nil && tabID != "" && isBridgeTabLostError(err) {
		// "No tab" is an authoritative disappearance. Clear every tab-id keyed
		// cache before returning so Chromium cannot later reuse the numeric id and
		// inherit ownership/cursors/emulation from the destroyed target.
		b.invalidateTabState(tabID)
		// A context pin is authoritative (explicit caller tab_id or an HTTP
		// session lease). Never strip it and fall through to the extension's
		// mutable active tab: in a shared daemon that would turn a closed leased
		// tab into an operation on another agent's or the human's tab.
		// Isolation mode applies the same fail-closed rule even for direct library
		// callers without a context pin; their NEXT call can auto-open a new owned
		// tab, but this failed call must never retry on the user's active tab.
		if browser.TabIDFromContext(ctx) != "" || !b.followFocus {
			return raw, err
		}
		delete(req, "tabId")
		return b.call(ctx, "cdp", req)
	}
	if err != nil && tabID != "" && isBridgeDebuggerDetachedError(err) {
		retryRaw, retryErr := b.call(ctx, "cdp", req)
		if retryErr == nil {
			return retryRaw, nil
		}
	}
	return raw, err
}
