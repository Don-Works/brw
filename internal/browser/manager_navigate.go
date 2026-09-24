package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// NavigateDirection enumerates the history navigation directions supported by
// the brw_navigate primitive. Generic, standards-based: it maps directly
// to the CDP Page navigation-history API (and to history.back/forward and
// location.reload on the extension bridge).
const (
	NavigateBack    = "back"
	NavigateForward = "forward"
	NavigateReload  = "reload"
)

// normalizeNavigateDirection lowercases/trims a direction and validates it
// against the supported set. Returns the canonical direction or an error.
func normalizeNavigateDirection(direction string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(direction))
	switch d {
	case NavigateBack, NavigateForward, NavigateReload:
		return d, nil
	default:
		return "", fmt.Errorf("direction must be one of back, forward, reload; got %q", direction)
	}
}

// Navigate moves through the active tab's session history (back/forward) or
// reloads the current document, then waits for load and returns a
// post-navigation observation. Implemented purely on web standards via the CDP
// Page domain: GetNavigationHistory + NavigateToHistoryEntry for back/forward
// and Reload for reload.
func (m *Manager) Navigate(ctx context.Context, direction string) (ActionResult, error) {
	if err := m.guardTakeover("navigate"); err != nil {
		return ActionResult{}, err
	}
	start := time.Now()
	dir, err := normalizeNavigateDirection(direction)
	if err != nil {
		return ActionResult{}, err
	}

	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	before := m.cachedBefore(tabID, tabCtx)

	// A history move is the agent's navigation even though the agent never named
	// the destination, so the intent is recorded without a host.
	m.recordAgentNavigation(tabID, "")
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		switch dir {
		case NavigateReload:
			return page.Reload().Do(ctx)
		default:
			return navigateHistory(ctx, dir)
		}
	})); err != nil {
		return ActionResult{}, err
	}

	// Wait for the destination document to settle before observing. A history
	// entry can resolve from bfcache instantly or trigger a full load, so the
	// caller-side WaitFor handles both via the in-page readiness promise.
	// "ready", not "load": what the observation needs is a document it can read,
	// and one slow subresource must not hold the result for the full timeout.
	_ = m.WaitFor(ctx, "ready", 10*time.Second)

	result := m.observeActionWithBefore(tabID, tabCtx, "navigated "+dir, before)
	result.DurationMS = time.Since(start).Milliseconds()
	m.recordTrace(tabID, TraceEntry{
		Action:     "navigate",
		Text:       dir,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

// NavigateTo navigates the active tab to a URL, waits for the page to load,
// and returns a post-navigation observation. Unlike Open, this does NOT create
// a new tab — it navigates the existing active tab.
func (m *Manager) NavigateTo(ctx context.Context, url string) (ActionResult, error) {
	if err := m.guardTakeover("navigate_to"); err != nil {
		return ActionResult{}, err
	}
	start := time.Now()
	var err error
	url, err = m.prepareNavigationURL(url)
	if err != nil {
		return ActionResult{}, fmt.Errorf("navigate_to: %w", err)
	}

	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	before := m.cachedBefore(tabID, tabCtx)

	// Record the destination BEFORE the navigation starts: the document request
	// arrives while this call is still in flight, and the content boundary has to
	// already know the agent asked for it.
	m.recordAgentNavigation(tabID, url)
	disarm := m.armInlineDocument(tabCtx, tabID, url)
	defer disarm()
	if err := chromedp.Run(tabCtx, chromedp.Navigate(url)); err != nil {
		if IsNavigationAbortedError(err) {
			return ActionResult{}, NavigationAbortedError("navigate_to")
		}
		if errorText, ok := strings.CutPrefix(err.Error(), "page load error "); ok {
			return ActionResult{}, &NavigationFailedError{
				Verb:          "navigate_to",
				Outcome:       m.navigationOutcome(tabID, url, "", errorText),
				AuthAvailable: true,
			}
		}
		return ActionResult{}, err
	}

	// Readiness, not the load event: the observation below needs a readable
	// document, not every subresource.
	_ = m.WaitFor(ctx, "ready", 10*time.Second)

	result := m.observeActionWithBefore(tabID, tabCtx, "navigated to "+url, before)
	result.DurationMS = time.Since(start).Milliseconds()
	// Redacted when the caller declared this navigation sensitive: a credentialed
	// URL is the kind that carries a token in its query string, and the trace is
	// replayable and readable long after the call.
	m.recordTrace(tabID, RedactTraceEntry(ctx, TraceEntry{
		Action:     "navigate_to",
		Text:       url,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	}))
	return result, nil
}

// navigateHistory walks one entry back or forward in the current page's
// session history using the CDP navigation-history API. It is a no-op error
// when there is no entry to move to in the requested direction.
func navigateHistory(ctx context.Context, dir string) error {
	currentIndex, entries, err := page.GetNavigationHistory().Do(ctx)
	if err != nil {
		return err
	}
	target := currentIndex
	switch dir {
	case NavigateBack:
		target = currentIndex - 1
	case NavigateForward:
		target = currentIndex + 1
	}
	if target < 0 || target >= int64(len(entries)) {
		return fmt.Errorf("no %s history entry available", dir)
	}
	entry := entries[target]
	if entry == nil {
		return errors.New("navigation history entry is nil")
	}
	return page.NavigateToHistoryEntry(entry.ID).Do(ctx)
}
