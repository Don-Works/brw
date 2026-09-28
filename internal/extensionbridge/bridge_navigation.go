package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func (b *Bridge) prepareNavigationURL(rawURL string) (string, error) {
	return b.navPolicy.CheckNavigation(rawURL)
}

func (b *Bridge) enforceFinalURL(ctx context.Context, rawURL string) error {
	if b.navPolicy.Empty() {
		return nil
	}
	if err := b.navPolicy.Check(rawURL); err != nil {
		blankJSON, _ := json.Marshal("about:blank")
		_, _ = b.cdp(ctx, "", "Runtime.evaluate", map[string]any{
			"expression":    fmt.Sprintf("location.replace(%s)", blankJSON),
			"returnByValue": true,
		})
		return fmt.Errorf("final browser destination rejected by navigation policy and reset to about:blank: %w", err)
	}
	return nil
}

func (b *Bridge) guardCurrentURL(ctx context.Context) error {
	if b.navPolicy.Empty() {
		return nil
	}
	raw, err := b.cdp(ctx, "", "Runtime.evaluate", map[string]any{
		"expression":    "location.href",
		"returnByValue": true,
	})
	if err != nil {
		return fmt.Errorf("verify current navigation destination: %w", err)
	}
	var payload struct {
		Result struct {
			Value string
		}
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("parse current navigation destination: %w", err)
	}
	return b.enforceFinalURL(ctx, payload.Result.Value)
}

func (b *Bridge) verifyOpenedTabURL(ctx context.Context, tabID string) error {
	tabs, err := b.ListTabs(ctx)
	if err != nil {
		if b.navPolicy.Empty() {
			return nil
		}
		return fmt.Errorf("verify open final destination: %w", err)
	}
	for _, tab := range tabs {
		if tab.ID != tabID {
			continue
		}
		if err := b.navPolicy.Check(tab.URL); err != nil {
			_ = b.CloseTab(ctx, tabID)
			return fmt.Errorf("open redirected to a disallowed final destination: %w", err)
		}
		return nil
	}
	if !b.navPolicy.Empty() {
		return fmt.Errorf("verify open final destination: tab %s disappeared", tabID)
	}
	return nil
}

// Navigate moves through the active tab's session history (back/forward) or
// reloads the current document via the in-page History/Location web APIs, then
// returns a post-navigation observation. Standards-only: history.back(),
// history.forward(), location.reload().
func (b *Bridge) Navigate(ctx context.Context, direction string) (browser.ActionResult, error) {
	dir, err := normalizeNavigateDirection(direction)
	if err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	if err := b.navigateDirection(ctx, dir); err != nil {
		return browser.ActionResult{}, err
	}
	// A history move / reload may tear down and rebuild the document; give it a
	// moment to settle, then wait for readiness before observing.
	b.settle(ctx, observedActionSettle)
	_ = b.WaitFor(ctx, "ready", 10*time.Second)
	return b.observeActionWithBefore(ctx, "navigated "+dir, before), nil
}

// NavigateTo navigates the active tab to a URL, waits for the page to load,
// and returns a post-navigation observation. Unlike Open, this does NOT create
// a new tab — it navigates the existing active tab.
func (b *Bridge) NavigateTo(ctx context.Context, url string) (browser.ActionResult, error) {
	var err error
	url, err = b.prepareNavigationURL(url)
	if err != nil {
		return browser.ActionResult{}, fmt.Errorf("navigate_to: %w", err)
	}
	// Navigation completion and the observation must stay on the same tab even
	// if the user changes focus while the replacement document is loading.
	ctx = b.pinActiveTab(ctx)
	// Arm before navigating: the in-page guard only beats page scripts for a
	// document that has not loaded yet.
	b.ensureContainment(ctx, b.contextTabID(ctx))
	b.ensureWebMCP(ctx, b.contextTabID(ctx))
	before := b.captureSemanticState(ctx)
	before.Trace = browser.TraceEntry{Action: "navigate_to", Text: url}
	beforeTabs := b.captureTabIDs(ctx)
	if err := b.navigateToURLAndWait(ctx, url); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBeforeAndTabs(ctx, "navigated to "+url, before, beforeTabs), nil
}

const navigationCommitTimeout = 10 * time.Second

type extensionDocumentIdentityPayload struct {
	DocumentID     string `json:"document_id"`
	DocumentEpoch  uint64 `json:"document_epoch"`
	WorkerInstance string `json:"worker_instance"`
	Origin         string `json:"origin"`
	TabID          int64  `json:"tab_id"`
}

type bridgeMainFrameState struct {
	ID       string
	LoaderID string
	URL      string
}

// navigateToURLAndWait pre-arms a trusted main-document identity before asking
// CDP to navigate, then waits for that exact tab to commit a replacement
// document. A generic "committed" wait is insufficient: the source document is
// already interactive and can satisfy it before location.href begins loading,
// allowing the next batch step to act on the old page. Page.navigate supplies a
// target loader id for replacement navigations; the independent webNavigation
// document id catches redirects/BFCache and protects against a stale source
// document. Same-document fragment navigations legitimately keep both ids and
// complete only when the main-frame URL reaches the requested target. An exact
// current-URL request is idempotent: it skips Page.navigate (callers that need a
// reload have the explicit navigate/reload action), but still requires the
// current document to become ready and remain the same trusted document.
func (b *Bridge) navigateToURLAndWait(ctx context.Context, targetURL string) error {
	tabID := b.contextTabID(ctx)
	if tabID == "" {
		return errors.New("navigate_to: no active tab available")
	}
	// Freeze every probe and command to this tab even in follow-focus mode.
	navCtx := browser.WithTabID(ctx, tabID)
	commitCtx, cancelCommit := context.WithTimeout(navCtx, navigationCommitTimeout)
	defer cancelCommit()
	beforeIdentity, identityErr := b.extensionDocumentIdentity(commitCtx)
	beforeFrame, frameErr := b.mainFrameState(commitCtx, tabID)
	if identityErr != nil || frameErr != nil {
		// A tab whose only navigation became a download (or was aborted) holds
		// the initial empty document: no committed URL, so no trusted identity
		// and no loader to pre-arm against. It is still a valid place to
		// navigate from. The zero-value boundary makes the first committed
		// replacement count as the document change the loop waits for.
		if !b.tabHasNoCommittedDocument(commitCtx, tabID) {
			if identityErr != nil {
				return fmt.Errorf("navigate_to: pre-arm main-document identity: %w", identityErr)
			}
			return fmt.Errorf("navigate_to: pre-arm main-frame loader: %w", frameErr)
		}
		beforeIdentity, beforeFrame = extensionDocumentIdentityPayload{}, bridgeMainFrameState{}
	}
	// Page.navigate is not a reliable reload primitive for an exact-current URL:
	// Chromium may return a loader id without ever committing a replacement
	// document (for example when an app/service worker treats the request as an
	// already-satisfied navigation). Waiting for that loader then burns the full
	// commit timeout even though the requested destination is already active.
	// Page.getFrameTree can retain a differently serialized/stale URL for a SPA,
	// so use the active document's unforgeable location.href for this decision.
	// Do this before issuing Page.navigate so we cannot mistake its still-active
	// source document for completion while a real replacement is pending.
	currentURL, currentURLErr := b.mainDocumentURL(commitCtx, tabID)
	if currentURLErr == nil && currentURL == targetURL {
		beforeFrame.URL = currentURL
		return b.waitForAcceptedNavigationDestination(
			commitCtx, tabID, targetURL, beforeIdentity, beforeFrame, true,
		)
	}
	// A non-empty speculative loader may be ignored only for an exact
	// fragment-only transition. This is the narrow URL shape the platform defines
	// as same-document; a path/query/origin change must still commit the returned
	// replacement loader even if the old document transiently reports the target.
	sameDocumentTarget := currentURLErr == nil && isExactFragmentTransition(currentURL, targetURL)

	defer b.armInlineDocument(navCtx, tabID, targetURL)()
	raw, err := b.cdp(commitCtx, tabID, "Page.navigate", map[string]any{"url": targetURL})
	if err != nil {
		if browser.IsNavigationAbortedError(err) {
			return browser.NavigationAbortedError("navigate_to")
		}
		return fmt.Errorf("navigate_to: %w", err)
	}
	var started struct {
		LoaderID   string `json:"loaderId"`
		ErrorText  string `json:"errorText"`
		IsDownload bool   `json:"isDownload"`
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		return fmt.Errorf("navigate_to: parse Page.navigate response: %w", err)
	}
	if errorText := strings.TrimSpace(started.ErrorText); errorText != "" {
		if strings.Contains(errorText, "net::ERR_ABORTED") {
			return browser.NavigationAbortedError("navigate_to")
		}
		return b.navigationFailure(navCtx, tabID, targetURL, "", errorText)
	}
	if started.IsDownload {
		return errors.New("navigate_to: destination started a download instead of replacing the page")
	}

	deadline, _ := commitCtx.Deadline()
	backoff := 25 * time.Millisecond
	for {
		if err := commitCtx.Err(); err != nil {
			return navigationCommitWaitError(err)
		}
		frame, frameErr := b.mainFrameState(commitCtx, tabID)
		identity, identityErr := b.extensionDocumentIdentity(commitCtx)
		if frameErr == nil && identityErr == nil {
			documentChanged := extensionNavigationDocumentChanged(beforeIdentity, identity)
			loaderMatched := started.LoaderID != "" && frame.LoaderID == started.LoaderID
			// Some SPAs update the live same-document location even though
			// Page.navigate returns a speculative non-empty loader id which never
			// commits. Accept that case only while BOTH trusted document identity and
			// the pre-armed frame/loader remain unchanged, and only after the pinned
			// document's exact location.href reaches the target. Page.getFrameTree's
			// URL is insufficient here because it can retain another SPA spelling.
			sameDocumentArrived := false
			if sameDocumentTarget && !documentChanged && frame.ID == beforeFrame.ID && frame.LoaderID == beforeFrame.LoaderID {
				if liveURL, err := b.mainDocumentURL(commitCtx, tabID); err == nil && liveURL == targetURL {
					// Close the probe race: a replacement can commit between the
					// frame/identity reads above and location.href. Re-read both boundaries
					// after the exact URL match and accept only if they are still the
					// original same-document values.
					confirmedFrame, frameConfirmErr := b.mainFrameState(commitCtx, tabID)
					confirmedIdentity, identityConfirmErr := b.extensionDocumentIdentity(commitCtx)
					if frameConfirmErr == nil && identityConfirmErr == nil &&
						!extensionNavigationDocumentChanged(beforeIdentity, confirmedIdentity) &&
						confirmedFrame.ID == beforeFrame.ID && confirmedFrame.LoaderID == beforeFrame.LoaderID {
						confirmedFrame.URL = liveURL
						frame = confirmedFrame
						identity = confirmedIdentity
						sameDocumentArrived = true
					}
				}
			}
			if (documentChanged && loaderMatched) || sameDocumentArrived {
				return b.waitForAcceptedNavigationDestination(
					commitCtx, tabID, targetURL, identity, frame, sameDocumentArrived,
				)
			}
		}
		if time.Now().After(deadline) {
			return errors.New("navigate_to: timed out waiting for the requested navigation to commit; the source document remained active")
		}
		timer := time.NewTimer(backoff)
		select {
		case <-commitCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return navigationCommitWaitError(commitCtx.Err())
		case <-timer.C:
		}
		if backoff < 200*time.Millisecond {
			backoff *= 2
			if backoff > 200*time.Millisecond {
				backoff = 200 * time.Millisecond
			}
		}
	}
}

func isExactFragmentTransition(before, after string) bool {
	if before == after {
		return false
	}
	beforeBase, _, beforeHasFragment := strings.Cut(before, "#")
	afterBase, _, afterHasFragment := strings.Cut(after, "#")
	return beforeBase == afterBase && (beforeHasFragment || afterHasFragment)
}

func navigationCommitWaitError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("navigate_to: timed out waiting for the requested navigation to commit; the source document remained active")
	}
	return err
}

// waitForAcceptedNavigationDestination runs readiness only after a caller has
// established a trusted destination boundary, then proves that boundary did not
// change underneath the readiness check. requireExactURL is true for
// same-document and exact-current requests; replacement navigations may finish
// at a different policy-allowed URL after a legitimate redirect.
func (b *Bridge) waitForAcceptedNavigationDestination(
	ctx context.Context,
	tabID, targetURL string,
	acceptedIdentity extensionDocumentIdentityPayload,
	acceptedFrame bridgeMainFrameState,
	requireExactURL bool,
) error {
	// A failed navigation commits Chrome's error page as a real replacement
	// document, so the loop above accepts it. Say why instead of running the
	// policy check and readiness against chrome-error://.
	if browser.IsErrorPageURL(acceptedFrame.URL) {
		return b.navigationFailure(ctx, tabID, targetURL, acceptedFrame.URL, "")
	}
	// Validate the accepted main-frame URL before executing even the readiness
	// predicate in that document. This closes the redirect gap without requiring
	// equality to targetURL for replacement navigations.
	if err := b.enforceFinalURL(ctx, acceptedFrame.URL); err != nil {
		return fmt.Errorf("navigate_to: %w", err)
	}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errors.New("navigate_to: destination accepted after the navigation deadline")
	}
	if err := b.WaitFor(ctx, "ready", remaining); err != nil {
		return fmt.Errorf("navigate_to: destination did not become ready: %w", err)
	}
	finalFrame, err := b.mainFrameState(ctx, tabID)
	if err != nil {
		return fmt.Errorf("navigate_to: verify accepted destination: %w", err)
	}
	finalIdentity, err := b.extensionDocumentIdentity(ctx)
	if err != nil {
		return fmt.Errorf("navigate_to: verify destination-document continuity: %w", err)
	}
	if extensionNavigationDocumentChanged(acceptedIdentity, finalIdentity) ||
		acceptedFrame.ID != finalFrame.ID || acceptedFrame.LoaderID != finalFrame.LoaderID {
		return errors.New("navigate_to: page changed again before the accepted destination became ready")
	}
	finalURL := finalFrame.URL
	if requireExactURL {
		// The frame-tree URL is not authoritative for a same-document SPA route;
		// verify exactness against the live document just as the preflight does.
		finalURL, err = b.mainDocumentURL(ctx, tabID)
		if err != nil {
			return fmt.Errorf("navigate_to: verify exact destination URL: %w", err)
		}
		if finalURL != targetURL {
			return errors.New("navigate_to: exact destination changed again before it became ready")
		}
	}
	if err := b.enforceFinalURL(ctx, finalURL); err != nil {
		return fmt.Errorf("navigate_to: %w", err)
	}
	return nil
}

// mainDocumentURL reads the actual top-level document URL without going through
// evaluateRuntime (whose navigation-policy guard intentionally performs another
// evaluation). Window.location is an unforgeable browser object, and CDP chooses
// the main-frame execution context when no context id is supplied.
func (b *Bridge) mainDocumentURL(ctx context.Context, tabID string) (string, error) {
	raw, err := b.cdp(ctx, tabID, "Runtime.evaluate", map[string]any{
		"expression":    "globalThis.location.href",
		"returnByValue": true,
	})
	if err != nil {
		return "", err
	}
	var payload struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails any `json:"exceptionDetails,omitempty"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	if payload.ExceptionDetails != nil {
		if msg := browser.FormatRuntimeException(payload.ExceptionDetails); msg != "" {
			return "", fmt.Errorf("runtime exception: %s", msg)
		}
		return "", errors.New("runtime exception while reading main-document URL")
	}
	if strings.TrimSpace(payload.Result.Value) == "" {
		return "", errMainDocumentURLUnavailable
	}
	return payload.Result.Value, nil
}

func extensionNavigationDocumentChanged(before, after extensionDocumentIdentityPayload) bool {
	if before.TabID != after.TabID || before.DocumentID != after.DocumentID {
		return true
	}
	// Epoch detects BFCache A->B->A reuse while one worker stays alive. A worker
	// restart resets the epoch, so do not mistake that reset alone for navigation;
	// webNavigation's documentId remains the stable cross-worker comparison.
	return before.WorkerInstance == after.WorkerInstance && before.DocumentEpoch != after.DocumentEpoch
}

func (b *Bridge) mainFrameState(ctx context.Context, tabID string) (bridgeMainFrameState, error) {
	raw, err := b.cdp(ctx, tabID, "Page.getFrameTree", nil)
	if err != nil {
		return bridgeMainFrameState{}, err
	}
	var payload struct {
		FrameTree struct {
			Frame struct {
				ID       string `json:"id"`
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return bridgeMainFrameState{}, err
	}
	frame := payload.FrameTree.Frame
	if strings.TrimSpace(frame.ID) == "" || strings.TrimSpace(frame.LoaderID) == "" || strings.TrimSpace(frame.URL) == "" {
		return bridgeMainFrameState{}, errors.New("main-frame state is unavailable")
	}
	return bridgeMainFrameState{ID: frame.ID, LoaderID: frame.LoaderID, URL: frame.URL}, nil
}

// tabHasNoCommittedDocument reports whether the tab still shows the initial
// empty document: location.href is about:blank or unreadable while chrome.tabs
// and the frame tree report no URL. That is the state a tab is left in when its
// only navigation turned into a download.
func (b *Bridge) tabHasNoCommittedDocument(ctx context.Context, tabID string) bool {
	liveURL, err := b.mainDocumentURL(ctx, tabID)
	if err != nil {
		return errors.Is(err, errMainDocumentURLUnavailable)
	}
	return liveURL == "about:blank"
}

// armInlineDocument asks the extension to pause the tab's next main-document
// response from destination's origin and rewrite a download-shaped text
// response so it renders as a page (see browser.InlineDocumentHeaders). Best
// effort: an extension that predates the message leaves navigation exactly as it
// was, and one that predates the url param pauses every document. The returned
// function releases the arm and is safe to call after ctx has ended.
func (b *Bridge) armInlineDocument(ctx context.Context, tabID, destination string) func() {
	if strings.TrimSpace(tabID) == "" {
		return func() {}
	}
	params := map[string]any{"tabId": parseTabID(tabID), "url": destination}
	if _, err := b.call(ctx, "arm_inline_document", params); err != nil {
		return func() {}
	}
	return func() {
		b.disarmInlineDocument(tabID)
	}
}

func (b *Bridge) disarmInlineDocument(tabID string) {
	if strings.TrimSpace(tabID) == "" {
		return
	}
	disarmCtx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), tabID), inlineDocumentDisarmTimeout)
	defer cancel()
	_, _ = b.call(disarmCtx, "disarm_inline_document", map[string]any{"tabId": parseTabID(tabID)})
}

const inlineDocumentDisarmTimeout = 5 * time.Second

var errMainDocumentURLUnavailable = errors.New("main-document URL is unavailable")

func (b *Bridge) navigateDirection(ctx context.Context, dir string) error {
	var expr string
	switch dir {
	case navigateBack:
		expr = "(function(){history.back();return true;})()"
	case navigateForward:
		expr = "(function(){history.forward();return true;})()"
	case navigateReload:
		expr = "(function(){location.reload();return true;})()"
	default:
		return fmt.Errorf("direction must be one of back, forward, reload; got %q", dir)
	}
	var ok bool
	if err := b.evaluate(ctx, expr, "", &ok); err != nil {
		// A reload/history move can destroy the execution context mid-evaluate;
		// that is the expected outcome of navigation, not a failure.
		if isNavigationTeardownError(err) {
			return nil
		}
		return err
	}
	return nil
}

const (
	navigateBack    = "back"
	navigateForward = "forward"
	navigateReload  = "reload"
)

func normalizeNavigateDirection(direction string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(direction))
	switch d {
	case navigateBack, navigateForward, navigateReload:
		return d, nil
	default:
		return "", fmt.Errorf("direction must be one of back, forward, reload; got %q", direction)
	}
}

func isNavigationTeardownError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "execution context was destroyed") ||
		strings.Contains(msg, "cannot find context with specified id") ||
		strings.Contains(msg, "frame was detached") ||
		strings.Contains(msg, "no by-value result")
}
