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
	_ = m.WaitFor(ctx, "load", 10*time.Second)

	result := m.observeActionWithBefore(tabID, tabCtx, "navigated "+dir, before)
	result.DurationMS = time.Since(start).Milliseconds()
	m.recordTrace(TraceEntry{
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
	start := time.Now()
	if strings.TrimSpace(url) == "" {
		return ActionResult{}, fmt.Errorf("navigate_to: url is required")
	}
	if !strings.Contains(url, "://") {
		url = "https://" + url
	}

	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	before := m.cachedBefore(tabID, tabCtx)

	if err := chromedp.Run(tabCtx, chromedp.Navigate(url)); err != nil {
		return ActionResult{}, err
	}

	_ = m.WaitFor(ctx, "load", 10*time.Second)

	result := m.observeActionWithBefore(tabID, tabCtx, "navigated to "+url, before)
	result.DurationMS = time.Since(start).Milliseconds()
	m.recordTrace(TraceEntry{
		Action:     "navigate_to",
		Text:       url,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
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
