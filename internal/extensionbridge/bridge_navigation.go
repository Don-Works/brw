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
	if check := browser.FrameReadCheckFromContext(ctx); check != nil {
		if err := check(rawURL); err != nil {
			return err
		}
	}
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
	if b.navPolicy.Empty() && browser.FrameReadCheckFromContext(ctx) == nil {
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
	if strings.TrimSpace(payload.Result.Value) == "" {
		return errors.New("current navigation destination is unavailable")
	}
	return b.enforceFinalURL(ctx, payload.Result.Value)
}

func (b *Bridge) verifyOpenedTabURL(ctx context.Context, tabID string) error {
	tabs, err := b.ListTabs(ctx)
	if err != nil {
		if b.navPolicy.Empty() && browser.FrameReadCheckFromContext(ctx) == nil {
			return nil
		}
		return fmt.Errorf("verify open final destination: %w", err)
	}
	for _, tab := range tabs {
		if tab.ID != tabID {
			continue
		}
		if err := b.enforceFinalURL(browser.WithTabID(ctx, tabID), tab.URL); err != nil {
			_ = b.CloseTab(ctx, tabID)
			return fmt.Errorf("open redirected to a disallowed final destination: %w", err)
		}
		return nil
	}
	if !b.navPolicy.Empty() || browser.FrameReadCheckFromContext(ctx) != nil {
		return fmt.Errorf("verify open final destination: tab %s disappeared", tabID)
	}
	return nil
}

// Navigate goes back, forward or reloads via history.back/forward and location.reload, then returns a post-navigation observation.
func (b *Bridge) Navigate(ctx context.Context, direction string) (browser.ActionResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return browser.ActionResult{}, err
	}
	dir, err := normalizeNavigateDirection(direction)
	if err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	if err := b.navigateDirection(ctx, dir); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	_ = b.WaitFor(ctx, "ready", 10*time.Second)
	return b.observeActionWithBefore(ctx, "navigated "+dir, before), nil
}

// NavigateTo navigates the active tab (no new tab) to url, waits for load and returns a post-navigation observation.
func (b *Bridge) NavigateTo(ctx context.Context, url string) (browser.ActionResult, error) {
	if err := b.pacer.BeforeAction(ctx, browser.TabIDFromContext(ctx)); err != nil {
		return browser.ActionResult{}, err
	}
	var err error
	url, err = b.prepareNavigationURL(url)
	if err != nil {
		return browser.ActionResult{}, fmt.Errorf("navigate_to: %w", err)
	}

	ctx = b.pinActiveTab(ctx)

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

func (b *Bridge) navigateToURLAndWait(ctx context.Context, targetURL string) error {
	tabID := b.contextTabID(ctx)
	if tabID == "" {
		return errors.New("navigate_to: no active tab available")
	}

	navCtx := browser.WithTabID(ctx, tabID)
	commitCtx, cancelCommit := context.WithTimeout(navCtx, navigationCommitTimeout)
	defer cancelCommit()
	beforeIdentity, identityErr := b.extensionDocumentIdentity(commitCtx)
	beforeFrame, frameErr := b.mainFrameState(commitCtx, tabID)
	if identityErr != nil || frameErr != nil {
		if !b.tabHasNoCommittedDocument(commitCtx, tabID) {
			if identityErr != nil {
				return fmt.Errorf("navigate_to: pre-arm main-document identity: %w", identityErr)
			}
			return fmt.Errorf("navigate_to: pre-arm main-frame loader: %w", frameErr)
		}
		beforeIdentity, beforeFrame = extensionDocumentIdentityPayload{}, bridgeMainFrameState{}
	}

	currentURL, currentURLErr := b.mainDocumentURL(commitCtx, tabID)
	if currentURLErr == nil && currentURL == targetURL {
		beforeFrame.URL = currentURL
		return b.waitForAcceptedNavigationDestination(
			commitCtx, tabID, targetURL, beforeIdentity, beforeFrame, true,
		)
	}

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

			sameDocumentArrived := false
			if sameDocumentTarget && !documentChanged && frame.ID == beforeFrame.ID && frame.LoaderID == beforeFrame.LoaderID {
				if liveURL, err := b.mainDocumentURL(commitCtx, tabID); err == nil && liveURL == targetURL {
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

func (b *Bridge) waitForAcceptedNavigationDestination(
	ctx context.Context,
	tabID, targetURL string,
	acceptedIdentity extensionDocumentIdentityPayload,
	acceptedFrame bridgeMainFrameState,
	requireExactURL bool,
) error {
	if browser.IsErrorPageURL(acceptedFrame.URL) {
		return b.navigationFailure(ctx, tabID, targetURL, acceptedFrame.URL, "")
	}

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

func (b *Bridge) tabHasNoCommittedDocument(ctx context.Context, tabID string) bool {
	liveURL, err := b.mainDocumentURL(ctx, tabID)
	if err != nil {
		return errors.Is(err, errMainDocumentURLUnavailable)
	}
	return liveURL == "about:blank"
}

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
