package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// validNotifyKinds is the closed, generic set of hand-off classifications the
// notify primitive understands. They are web-standard semantic categories, not
// site-specific behaviours.
var validNotifyKinds = map[string]struct{}{
	"needs_input": {},
	"done":        {},
	"error":       {},
}

// NormalizeNotifyOptions validates and fills defaults for a NotifyOptions. It
// is shared by every transport (direct-CDP Manager and the extension Bridge)
// so the contract is identical regardless of how the notification is
// ultimately raised.
func NormalizeNotifyOptions(opts NotifyOptions) (NotifyOptions, error) {
	opts.Kind = strings.TrimSpace(strings.ToLower(opts.Kind))
	if opts.Kind == "" {
		opts.Kind = "needs_input"
	}
	if _, ok := validNotifyKinds[opts.Kind]; !ok {
		return opts, fmt.Errorf("invalid notify kind %q: want needs_input, done, or error", opts.Kind)
	}
	opts.Title = strings.TrimSpace(opts.Title)
	if opts.Title == "" {
		opts.Title = defaultNotifyTitle(opts.Kind)
	}
	opts.Message = strings.TrimSpace(opts.Message)
	return opts, nil
}

func defaultNotifyTitle(kind string) string {
	switch kind {
	case "done":
		return "agent-browser: done"
	case "error":
		return "agent-browser: error"
	default:
		return "agent-browser: action needed"
	}
}

// PageNotifyScript raises an in-page Notification (best-effort). It runs in the
// page context via direct CDP when no extension bridge is available. The
// browser's Notification permission, focus, and OS policy all gate whether the
// notification is actually shown, so the result reports honestly rather than
// claiming success. Returns {ok, delivery, note}.
const PageNotifyScript = `(function(opts) {
  try {
    if (typeof Notification === 'undefined') {
      return { ok: false, delivery: 'unavailable', note: 'Notification API not available in this page' };
    }
    var body = opts.message || '';
    function fire() {
      try {
        new Notification(opts.title || 'agent-browser', { body: body, tag: 'agent-browser-' + (opts.kind || 'notify') });
        return { ok: true, delivery: 'page', note: 'raised via in-page Notification API (subject to page focus/permission)' };
      } catch (e) {
        return { ok: false, delivery: 'unavailable', note: 'Notification constructor failed: ' + (e && e.message ? e.message : String(e)) };
      }
    }
    if (Notification.permission === 'granted') {
      return fire();
    }
    if (Notification.permission === 'denied') {
      return { ok: false, delivery: 'unavailable', note: 'Notification permission denied for this origin' };
    }
    // permission === 'default': request asynchronously; we cannot await the
    // user gesture here, so report best-effort honestly instead of faking.
    try { Notification.requestPermission(); } catch (e) {}
    return { ok: false, delivery: 'unavailable', note: 'Notification permission not yet granted; requested from this origin' };
  } catch (e) {
    return { ok: false, delivery: 'unavailable', note: String(e && e.message ? e.message : e) };
  }
})`

// Notify raises a desktop notification at a human hand-off point. On a
// direct-CDP Manager (no extension) there is no chrome.notifications surface,
// so it falls back to the in-page Notification API on a best-effort basis and
// reports the honest delivery channel (never a fake success).
func (m *Manager) Notify(ctx context.Context, opts NotifyOptions) (NotifyResult, error) {
	opts, err := NormalizeNotifyOptions(opts)
	if err != nil {
		return NotifyResult{}, err
	}
	_, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return NotifyResult{}, err
	}
	defer cancel()

	optsJSON, _ := json.Marshal(map[string]any{
		"kind":    opts.Kind,
		"title":   opts.Title,
		"message": opts.Message,
	})
	var result NotifyResult
	expr := fmt.Sprintf("%s(%s)", PageNotifyScript, optsJSON)
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(expr, &result, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	})); err != nil {
		return NotifyResult{}, err
	}
	if result.Delivery == "" {
		result.Delivery = "unavailable"
	}
	return result, nil
}
