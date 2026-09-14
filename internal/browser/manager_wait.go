package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// RecentDownloadWindow is how far back a finished download still counts as
// belonging to the caller's most recent action. Long enough to cover a click
// whose download completes before the following wait call is issued, short
// enough that an unrelated download from earlier in the session does not
// satisfy the wait. Exported so the extension transport's polling fallback
// answers "is this the download my click caused?" with the same number.
const RecentDownloadWindow = 15 * time.Second

// RecentDialogWindow is the same idea for dialogs, and for the same reason: brw
// answers a dialog the instant Chrome opens it, so by the time the wait written
// after the click runs, the dialog is already open, answered and gone.
const RecentDialogWindow = 15 * time.Second

// WaitFor blocks until condition holds. It is the Controller-facing form; use
// WaitForOutcome when the caller wants to know HOW the wait resolved.
func (m *Manager) WaitFor(ctx context.Context, condition string, timeout time.Duration) error {
	_, err := m.WaitForOutcome(ctx, condition, timeout)
	return err
}

// WaitForOutcome resolves a wait and reports what resolved it.
//
// Load, dialog and download waits are answered from the tab's CDP event
// subscription (see events.go): the wait registers with a stream that is already
// running and blocks until the event arrives, so nothing in that code path asks
// the browser anything. The remaining conditions are page state that only the
// document can answer, and use one awaited in-page promise.
func (m *Manager) WaitForOutcome(ctx context.Context, condition string, timeout time.Duration) (WaitOutcome, error) {
	if timeout == 0 {
		timeout = m.timeout
	}
	started := time.Now()
	finish := func(outcome WaitOutcome, err error) (WaitOutcome, error) {
		outcome.Condition = condition
		outcome.OK = err == nil
		outcome.WaitedMS = time.Since(started).Milliseconds()
		return outcome, err
	}

	// A download never touches the DOM, so no in-page condition can observe it.
	// It resolves against the manager's own registry, woken by
	// Browser.downloadProgress.
	if match, ok := conditionArgument(condition, "download"); ok {
		return finish(m.waitForDownload(ctx, match, timeout))
	}

	// Buffer the Go-side context slightly beyond the in-page timeout so the
	// page's own timer resolves the wait before the CDP call is cancelled.
	tabID, tabCtx, cancel, err := m.activeContextWithTimeout(ctx, timeout+2*time.Second)
	if err != nil {
		return WaitOutcome{Condition: condition}, err
	}
	defer cancel()

	// A dialog blocks the renderer until it is answered, so an in-page predicate
	// cannot see one either.
	if match, ok := conditionArgument(condition, "dialog"); ok {
		return finish(m.waitForDialog(ctx, tabCtx, tabID, match, timeout))
	}
	if isReadinessCondition(condition) {
		if outcome, err, handled := m.waitForLoadEvent(ctx, tabCtx, tabID, condition, timeout); handled {
			return finish(outcome, err)
		}
	}
	return finish(m.waitForConditionScript(ctx, tabCtx, condition, timeout))
}

// conditionArgument splits "name" / "name:argument" for the conditions brw
// answers itself rather than in the page.
func conditionArgument(condition, name string) (string, bool) {
	if condition == name {
		return "", true
	}
	if rest, ok := strings.CutPrefix(condition, name+":"); ok {
		return rest, true
	}
	return "", false
}

// isReadinessCondition reports the aliases that mean "the document is usable".
// "load" is the load event itself; "ready"/"page_ready" are satisfied earlier,
// as soon as the document is interactive.
func isReadinessCondition(condition string) bool {
	switch condition {
	case "load", "ready", "page_ready", "":
		return true
	}
	return false
}

// waitForLoadEvent answers a readiness wait from Page.loadEventFired. The third
// return value reports whether it answered at all: when the hub has seen no
// lifecycle event for this tab, brw attached after the document had already
// loaded and no event is coming, so the caller must read the document instead.
func (m *Manager) waitForLoadEvent(ctx, tabCtx context.Context, tabID, condition string, timeout time.Duration) (WaitOutcome, error, bool) {
	observed, loaded := m.events.loadState(tabID)
	if !observed {
		return WaitOutcome{}, nil, false
	}
	if loaded {
		return WaitOutcome{ResolvedBy: WaitResolvedByEvent}, nil, true
	}
	// A document that is merely interactive satisfies "ready" before its load
	// event fires, and only the document knows that it is.
	if condition != "load" {
		return WaitOutcome{}, nil, false
	}

	sub, release := m.events.subscribe([]pageEventKind{eventLoad}, tabID)
	defer release()
	// The load may have landed between the state read above and the subscribe.
	if _, loaded := m.events.loadState(tabID); loaded {
		return WaitOutcome{ResolvedBy: WaitResolvedByEvent}, nil, true
	}

	outcome, err := awaitEvent(ctx, tabCtx, sub, timeout, condition, func(ev pageEvent) bool {
		return ev.Kind == eventLoad
	})
	return outcome, err, true
}

// waitForDialog resolves from Page.javascriptDialogOpening. match, when
// non-empty, is a case-insensitive substring tested against the dialog's message
// and its type, so a caller can wait for one specific prompt.
func (m *Manager) waitForDialog(ctx, tabCtx context.Context, tabID, match string, timeout time.Duration) (WaitOutcome, error) {
	needle := strings.ToLower(strings.TrimSpace(match))
	matches := func(ev pageEvent) bool {
		if ev.Kind != eventDialog {
			return false
		}
		if needle == "" {
			return true
		}
		return strings.Contains(strings.ToLower(ev.Text), needle) ||
			strings.Contains(strings.ToLower(ev.Detail), needle)
	}

	// Subscribe before reading the ring so a dialog opening between the two is
	// delivered rather than falling into the gap.
	sub, release := m.events.subscribe([]pageEventKind{eventDialog}, tabID)
	defer release()
	for _, ev := range m.events.recent(tabID, eventDialog, time.Now().Add(-RecentDialogWindow)) {
		if matches(ev) {
			return WaitOutcome{ResolvedBy: WaitResolvedByEvent}, nil
		}
	}
	return awaitEvent(ctx, tabCtx, sub, timeout, dialogConditionName(match), matches)
}

func dialogConditionName(match string) string {
	if strings.TrimSpace(match) == "" {
		return "dialog"
	}
	return "dialog:" + match
}

// awaitEvent blocks on the shared stream until an event matches, the caller
// cancels, the tab goes away, or the deadline passes. Every wake-up is an event
// that actually happened; nothing here re-asks the browser on a timer.
//
// tabCtx matters because the subscription dies with the tab: once the tab is
// gone no further event can arrive, so a wait that only watched its own deadline
// would sit there for the full timeout before reporting the wrong reason.
func awaitEvent(ctx, tabCtx context.Context, sub <-chan pageEvent, timeout time.Duration, condition string, matches func(pageEvent) bool) (WaitOutcome, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	wakeups := 0
	for {
		select {
		case <-ctx.Done():
			return WaitOutcome{Wakeups: wakeups}, fmt.Errorf("wait for %q cancelled", condition)
		case <-tabCtx.Done():
			return WaitOutcome{Wakeups: wakeups}, fmt.Errorf("wait for %q ended: the tab closed", condition)
		case <-deadline.C:
			return WaitOutcome{Wakeups: wakeups}, fmt.Errorf("timed out waiting for %q", condition)
		case ev := <-sub:
			wakeups++
			if matches(ev) {
				return WaitOutcome{ResolvedBy: WaitResolvedByEvent, Wakeups: wakeups}, nil
			}
		}
	}
}

// waitForConditionScript is the in-page path for conditions only the document
// can answer (text, url, selector, ref, fn). One awaited promise that resolves
// on the mutation or navigation event satisfying the predicate — not a poll —
// re-armed if a navigation destroys the execution context holding it.
func (m *Manager) waitForConditionScript(ctx context.Context, tabCtx context.Context, condition string, timeout time.Duration) (WaitOutcome, error) {
	deadline := time.Now().Add(timeout)
	attempts := 0
	for {
		// Cooperative cancellation: a brw_cancel on the surrounding plan/batch
		// (or this tab) cancels the caller-supplied ctx, which unblocks a long
		// wait promptly instead of running it out to the full timeout.
		if err := ctx.Err(); err != nil {
			return WaitOutcome{Wakeups: attempts}, fmt.Errorf("wait for %q cancelled", condition)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return WaitOutcome{Wakeups: attempts}, fmt.Errorf("timed out waiting for %q", condition)
		}
		attempts++
		matched, err := snapshot.WaitForCondition(tabCtx, condition, remaining.Milliseconds())
		if err == nil {
			if matched {
				return WaitOutcome{ResolvedBy: WaitResolvedByScript, Wakeups: attempts}, nil
			}
			return WaitOutcome{Wakeups: attempts}, fmt.Errorf("timed out waiting for %q", condition)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return WaitOutcome{Wakeups: attempts}, fmt.Errorf("wait for %q cancelled", condition)
		}
		if !isTransientNavigationError(err) {
			return WaitOutcome{Wakeups: attempts}, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForDownload blocks until a download that was not already finished when the
// wait started reaches the completed state. match, when non-empty, is a
// case-insensitive substring tested against the suggested filename and the URL,
// so a caller can wait for one specific file among several in flight.
//
// Only downloads that start (or are still running) after the baseline is taken
// can satisfy the wait. Without that, a wait placed after a click would return
// instantly on an unrelated download completed minutes earlier.
//
// The registry is re-read on every Browser.downloadProgress wake-up, never on a
// timer: the subscription that feeds the registry publishes only AFTER writing
// it, so a wake-up always carries the state change it reports.
func (m *Manager) waitForDownload(ctx context.Context, match string, timeout time.Duration) (WaitOutcome, error) {
	if timeout == 0 {
		timeout = m.timeout
	}
	if err := m.ensureDownloadTracking(ctx); err != nil {
		return WaitOutcome{}, err
	}
	needle := strings.ToLower(strings.TrimSpace(match))
	matches := func(entry DownloadEntry) bool {
		if needle == "" {
			return true
		}
		return strings.Contains(strings.ToLower(entry.SuggestedFilename), needle) ||
			strings.Contains(strings.ToLower(entry.URL), needle)
	}

	// Subscribe before the baseline so a completion landing between the two
	// wakes the wait instead of being missed by both.
	sub, release := m.events.subscribe([]pageEventKind{eventDownload}, browserEventScope)
	defer release()

	// Baseline: ignore downloads that finished before this action. A download
	// that completed moments ago is the one the caller just triggered — waits are
	// written after the click, and a small file frequently beats the wait — so it
	// counts, while anything older is treated as already dealt with.
	cutoff := time.Now().Add(-RecentDownloadWindow)
	settled := make(map[string]bool)
	m.downloadsMu.Lock()
	m.ensureDownloadMapsLocked()
	for _, entry := range m.downloads {
		terminal := entry.State == string(downloadStateCompleted) || entry.State == string(downloadStateCanceled)
		if terminal && m.downloadSettledBefore(entry.GUID, cutoff) {
			settled[entry.GUID] = true
		}
	}
	m.downloadsMu.Unlock()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	wakeups := 0
	for {
		if err := ctx.Err(); err != nil {
			return WaitOutcome{Wakeups: wakeups}, errors.New(`wait for "download" cancelled`)
		}
		var cancelled string
		m.downloadsMu.Lock()
		for _, entry := range m.downloads {
			if settled[entry.GUID] || !matches(entry) {
				continue
			}
			if entry.State == string(downloadStateCompleted) {
				m.downloadsMu.Unlock()
				return WaitOutcome{ResolvedBy: WaitResolvedByEvent, Wakeups: wakeups}, nil
			}
			if entry.State == string(downloadStateCanceled) {
				cancelled = entry.SuggestedFilename
			}
		}
		m.downloadsMu.Unlock()
		if cancelled != "" {
			return WaitOutcome{Wakeups: wakeups}, fmt.Errorf("download %q was cancelled by the browser before it completed", cancelled)
		}
		select {
		case <-ctx.Done():
			return WaitOutcome{Wakeups: wakeups}, errors.New(`wait for "download" cancelled`)
		case <-deadline.C:
			if needle == "" {
				return WaitOutcome{Wakeups: wakeups}, errors.New("timed out waiting for a download to complete")
			}
			return WaitOutcome{Wakeups: wakeups}, fmt.Errorf("timed out waiting for a download matching %q to complete", match)
		case <-sub:
			wakeups++
		}
	}
}
