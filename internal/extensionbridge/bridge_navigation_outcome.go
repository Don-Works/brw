package extensionbridge

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

type extensionNavigationOutcome struct {
	Known        bool     `json:"known"`
	URL          string   `json:"url"`
	Status       int      `json:"status"`
	Authenticate []string `json:"authenticate"`
	Error        string   `json:"error"`
}

const navigationOutcomeSettle = 500 * time.Millisecond

const navigationOutcomeCallTimeout = 2 * time.Second

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

func (b *Bridge) openResult(ctx context.Context, tab browser.Tab, requestedURL string, ready bool) browser.OpenResult {
	result := browser.OpenResult{Tab: tab, Ready: ready}
	if requestedURL == "about:blank" {
		return result
	}
	result.ApplyNavigationOutcome(b.navigationOutcome(ctx, tab.ID, requestedURL, ""), false)
	return result
}

func (b *Bridge) navigationFailure(ctx context.Context, tabID, targetURL, committedURL, errorText string) error {
	outcome := b.navigationOutcome(ctx, tabID, targetURL, committedURL)
	if outcome.Error == "" {
		outcome.Error = errorText
	}
	return &browser.NavigationFailedError{Verb: "navigate_to", Outcome: outcome}
}
