package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// HistoryStateOptions describes a same-document history change.
type HistoryStateOptions struct {
	URL string `json:"url"`
	// State is the JSON serialization of the history state object. Empty pushes
	// null, which is what a router that reads only location sees anyway.
	State string `json:"state,omitempty"`
	// Replace swaps the current entry (history.replaceState) instead of adding
	// one, for testing a redirect a router performs in place.
	Replace bool `json:"replace,omitempty"`
	// Notify dispatches a popstate event after the change. Defaults to true;
	// pass false to change the URL and observe a router that does NOT react.
	Notify *bool  `json:"notify,omitempty"`
	TabID  string `json:"tab_id,omitempty"`
}

// HistoryStateResult reports the committed same-document URL.
type HistoryStateResult struct {
	OK       bool   `json:"ok"`
	URL      string `json:"url"`
	Previous string `json:"previous_url,omitempty"`
	Replaced bool   `json:"replaced"`
	Notified bool   `json:"notified"`
	// Length is history.length after the change: one more than before for a
	// push, unchanged for a replace.
	Length int `json:"history_length,omitempty"`
}

// pushStateScript performs the same-document history change and reports the
// resulting URL.
//
// It dispatches popstate afterwards by default because pushState alone changes
// the address bar and nothing else — the History API deliberately does not fire
// popstate for a programmatic push. A client-side router that only subscribes to
// popstate would therefore show its old view under a new URL, and the caller
// would be testing nothing. Routers that patch history.pushState itself see both
// signals, which is the same thing they see from a back/forward gesture.
const pushStateScript = `(function(url, state, replace, notify){
  var previous = location.href;
  var parsed = null;
  if (state) {
    try { parsed = JSON.parse(state); }
    catch (e) { throw new Error('state must be JSON: ' + e.message); }
  }
  if (replace) { history.replaceState(parsed, '', url); }
  else { history.pushState(parsed, '', url); }
  if (notify) {
    var event;
    try { event = new PopStateEvent('popstate', {state: history.state}); }
    catch (e) { event = new Event('popstate'); }
    window.dispatchEvent(event);
  }
  return {ok:true, url: location.href, previous: previous, history_length: history.length};
})`

// PushState changes the page URL through the History API WITHOUT loading a new
// document, which is how a client-side router is driven directly: the JS heap,
// open websockets and in-memory state all survive, and only the route changes.
//
// The target passes the SAME navigation policy a real navigation does. A
// same-document history change is still a destination the agent chose, and
// pushState is a plausible way to reach a route the operator put off the
// allowlist, so the policy is checked on the URL resolved against the current
// document — not on the relative string the caller passed.
//
// Direct-CDP transport only.
func (m *Manager) PushState(ctx context.Context, opts HistoryStateOptions) (HistoryStateResult, error) {
	if strings.TrimSpace(opts.URL) == "" {
		return HistoryStateResult{}, errors.New("url is required")
	}
	start := time.Now()
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return HistoryStateResult{}, err
	}
	defer cancel()

	var current string
	if err := chromedp.Run(tabCtx, chromedp.Evaluate("location.href", &current)); err != nil {
		return HistoryStateResult{}, fmt.Errorf("read current document URL: %w", err)
	}
	resolved, err := ResolveHistoryTarget(current, opts.URL)
	if err != nil {
		return HistoryStateResult{}, err
	}
	// Policy first, origin second: an off-allowlist target must be refused with
	// the policy's own reason, not masked by the API's same-origin rule.
	//
	// Do not read this as the containment guarantee. While the current document
	// is itself on-policy the check refuses nothing the same-origin rule below
	// would not: a same-origin target shares the current host and so passes an
	// allowlist by construction, and a cross-origin one fails both. It adds a
	// refusal only when the tab is already on an OFF-policy document — a page
	// opened before the policy was set — and the URL guard evicts such a tab to
	// about:blank as soon as anything observes it, so this is the backstop for
	// the window before that, not the containment boundary itself
	// (TestPushStateRefusedFromABlockedDocument covers it). What it always buys
	// is the truthful reason.
	if err := m.navPolicy.Check(resolved); err != nil {
		return HistoryStateResult{}, fmt.Errorf("pushstate refused by navigation policy: %w", err)
	}
	if err := sameDocumentOrigin(current, resolved); err != nil {
		return HistoryStateResult{}, err
	}

	notify := true
	if opts.Notify != nil {
		notify = *opts.Notify
	}
	urlJSON, _ := json.Marshal(resolved)
	stateJSON, _ := json.Marshal(opts.State)
	replaceJSON, _ := json.Marshal(opts.Replace)
	notifyJSON, _ := json.Marshal(notify)
	expr := fmt.Sprintf("%s(%s,%s,%s,%s)", pushStateScript, urlJSON, stateJSON, replaceJSON, notifyJSON)

	var out struct {
		OK       bool   `json:"ok"`
		URL      string `json:"url"`
		Previous string `json:"previous"`
		Length   int    `json:"history_length"`
	}
	if err := runWithPrearmedSettle(tabCtx, actionSettleDelayFast, func() error {
		return chromedp.Run(tabCtx, chromedp.Evaluate(expr, &out))
	}); err != nil {
		return HistoryStateResult{}, err
	}
	// A router is free to redirect the route it was just handed, so the URL that
	// actually committed is re-checked before it is reported as reached.
	if err := m.enforceFinalURL(tabID, tabCtx, out.URL); err != nil {
		return HistoryStateResult{}, err
	}
	m.invalidateState(tabID)
	m.recordTrace(tabID, TraceEntry{
		Action:     "pushstate",
		Value:      out.URL,
		OK:         out.OK,
		DurationMS: time.Since(start).Milliseconds(),
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return HistoryStateResult{
		OK:       out.OK,
		URL:      out.URL,
		Previous: out.Previous,
		Replaced: opts.Replace,
		Notified: notify,
		Length:   out.Length,
	}, nil
}

// ResolveHistoryTarget resolves a pushState target against the document it will
// be applied to, so the policy check and the same-origin check both see the
// absolute URL the browser will commit rather than "/app/route".
func ResolveHistoryTarget(currentURL, target string) (string, error) {
	base, err := url.Parse(currentURL)
	if err != nil {
		return "", fmt.Errorf("current document URL %q is unparseable: %w", currentURL, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return "", fmt.Errorf("pushstate needs a page loaded over http(s); this tab is on %q", currentURL)
	}
	ref, err := url.Parse(strings.TrimSpace(target))
	if err != nil {
		return "", fmt.Errorf("invalid pushstate url %q: %w", target, err)
	}
	return base.ResolveReference(ref).String(), nil
}

// sameDocumentOrigin rejects a cross-origin target before the page does. The
// History API throws a SecurityError for one, and that exception reaches the
// caller as an opaque script failure rather than an explanation.
func sameDocumentOrigin(currentURL, target string) error {
	current, err := url.Parse(currentURL)
	if err != nil {
		return fmt.Errorf("current document URL %q is unparseable: %w", currentURL, err)
	}
	next, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("invalid pushstate url %q: %w", target, err)
	}
	if current.Scheme != next.Scheme || current.Host != next.Host {
		return fmt.Errorf("pushstate cannot change origin: the page is on %s://%s and %q is a different origin — a same-document history change is confined to one origin, so use brw_navigate_to for a real navigation", current.Scheme, current.Host, target)
	}
	return nil
}
