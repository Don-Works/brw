package extensionbridge

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// extensionNavigationOutcome is the extension's navigation_outcome reply: what
// the last navigation the daemon armed on a tab ended with.
type extensionNavigationOutcome struct {
	Known        bool     `json:"known"`
	URL          string   `json:"url"`
	Status       int      `json:"status"`
	Authenticate []string `json:"authenticate"`
	Error        string   `json:"error"`
}

// navigationOutcomeSettle bounds the wait for webNavigation.onErrorOccurred
// after Chrome has already committed its error page.
const navigationOutcomeSettle = 500 * time.Millisecond

// navigationOutcomeCallTimeout bounds each call that reads the outcome. While
// the document request is still waiting for the server, Chrome answers no
// debugger command on the tab, and the bridge's default call timeout would
// hold the open that long after its readiness wait gave up.
const navigationOutcomeCallTimeout = 2 * time.Second

// navigationOutcome reports how the tab's last brw-driven navigation ended.
// committedURL is the main frame's URL when the caller has it; empty reads it
// here. An extension older than navigation_outcome still yields the committed
// URL, which is enough to tell a failed navigation from a loaded page.
func (b *Bridge) navigationOutcome(ctx context.Context, tabID, requestedURL, committedURL string) browser.NavigationOutcome {
	out := browser.NavigationOutcome{URL: requestedURL, CommittedURL: committedURL}
	if out.CommittedURL == "" {
		frameCtx, cancel := context.WithTimeout(ctx, navigationOutcomeCallTimeout)
		frame, err := b.mainFrameState(frameCtx, tabID)
		cancel()
		if err == nil {
			out.CommittedURL = frame.URL
		}
	}
	deadline := time.Now().Add(navigationOutcomeSettle)
	for {
		ext, ok := b.readNavigationOutcome(ctx, tabID)
		if !ok {
			return out
		}
		out.HTTPStatus = ext.Status
		out.AuthRequired = browser.ParseAuthChallenge(ext.Authenticate)
		out.Error = ext.Error
		if out.Error != "" || !browser.IsErrorPageURL(out.CommittedURL) || time.Now().After(deadline) {
			return out
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out
		case <-timer.C:
		}
	}
}

func (b *Bridge) readNavigationOutcome(ctx context.Context, tabID string) (extensionNavigationOutcome, bool) {
	callCtx, cancel := context.WithTimeout(ctx, navigationOutcomeCallTimeout)
	defer cancel()
	raw, err := b.call(callCtx, "navigation_outcome", map[string]any{"tabId": parseTabID(tabID)})
	if err != nil {
		return extensionNavigationOutcome{}, false
	}
	var ext extensionNavigationOutcome
	if json.Unmarshal(raw, &ext) != nil || !ext.Known {
		return extensionNavigationOutcome{}, false
	}
	return ext, true
}

// openResult builds an open's result and folds in how its navigation ended.
// The extension bridge has no brw_authenticate, so an auth challenge names the
// CDP lane as the way past it.
func (b *Bridge) openResult(ctx context.Context, tab browser.Tab, requestedURL string, ready bool) browser.OpenResult {
	result := browser.OpenResult{Tab: tab, Ready: ready}
	if requestedURL == "about:blank" {
		return result
	}
	result.ApplyNavigationOutcome(b.navigationOutcome(ctx, tab.ID, requestedURL, ""), false)
	return result
}

// navigationFailure is the error navigate_to returns when the navigation ended
// on Chrome's error page or a network error.
func (b *Bridge) navigationFailure(ctx context.Context, tabID, targetURL, committedURL, errorText string) error {
	outcome := b.navigationOutcome(ctx, tabID, targetURL, committedURL)
	if outcome.Error == "" {
		outcome.Error = errorText
	}
	return &browser.NavigationFailedError{Verb: "navigate_to", Outcome: outcome}
}
