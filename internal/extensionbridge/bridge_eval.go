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
	// The expression can carry a sensitive recipe value, so it is redacted like
	// input actions before recording.
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
	// Raw top-level await is a syntax error in normal mode; retry only that in
	// REPL mode, which would otherwise return an IIFE's Promise unawaited
	// (breaking wait_for and settle).
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

// evaluateReadOnly retries a renderer-context rollover (a new popup can commit
// between target discovery and Runtime.evaluate). Mutating evaluations must
// not use it: a replay could duplicate the action.
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
		// A void result (location.reload(), assignments) is success; surface JSON null.
		if err := json.Unmarshal([]byte("null"), dst); err != nil {
			return err
		}
	} else if err := json.Unmarshal(payload.Result.Value, dst); err != nil {
		return err
	}
	return b.guardCurrentURL(ctx)
}

func (b *Bridge) cdpDispatch(ctx context.Context, tabID, method string, params map[string]any) (json.RawMessage, error) {
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
		// "No tab" is authoritative: clear tab-keyed caches so a reused id inherits nothing.
		b.invalidateTabState(tabID)
		// A context pin (explicit tab_id or lease), or isolation mode, must never fall
		// through to the mutable active tab, which may be another agent's or the human's.
		if browser.TabIDFromContext(ctx) != "" || !b.followFocus {
			return raw, err
		}
		delete(req, "tabId")
		return b.call(ctx, "cdp", req)
	}
	if err != nil && tabID != "" && isBridgeDebuggerDetachedError(err) && !ambiguousInputAcknowledgement(method, params) {
		retryRaw, retryErr := b.call(ctx, "cdp", req)
		if retryErr == nil {
			return retryRaw, nil
		}
	}
	return raw, err
}

func ambiguousInputAcknowledgement(method string, params map[string]any) bool {
	return method == "Input.dispatchTouchEvent" || method == "Input.dispatchMouseEvent" && (params["type"] == "mousePressed" || params["type"] == "mouseReleased")
}
