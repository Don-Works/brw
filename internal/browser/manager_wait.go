package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// RecentDownloadWindow is how far back a finished download still counts as belonging to the caller's most recent action.
const RecentDownloadWindow = 15 * time.Second

// RecentDialogWindow is the same idea for dialogs, and for the same reason: brw answers a dialog the instant Chrome opens it, so by the time the wait written after the click runs, the dialog is already open, answered and gone.
const RecentDialogWindow = 15 * time.Second

// WaitFor blocks until condition holds.
func (m *Manager) WaitFor(ctx context.Context, condition string, timeout time.Duration) error {

	_, err := m.WaitForOutcome(ctx, condition, timeout)
	return err
}

// WaitForOutcome resolves a wait and reports what resolved it.
func (m *Manager) WaitForOutcome(ctx context.Context, condition string, timeout time.Duration) (WaitOutcome, error) {
	if err := GuardCrossOriginRefs("wait for", DirectCrossOriginRemedy, snapshot.WaitConditionRef(condition)); err != nil {
		return WaitOutcome{}, err
	}
	if timeout == 0 {
		timeout = m.timeout
	}
	started := time.Now()
	var currentTabCtx context.Context
	var currentTabID string
	finish := func(outcome WaitOutcome, err error) (WaitOutcome, error) {
		if currentTabCtx != nil && FrameReadCheckFromContext(currentTabCtx) != nil {
			if guardErr := m.guardCurrentURL(currentTabID, currentTabCtx); guardErr != nil {
				return WaitOutcome{}, guardErr
			}
			if err != nil {
				return WaitOutcome{}, err
			}
		}
		outcome.Condition = condition
		outcome.OK = err == nil
		outcome.WaitedMS = time.Since(started).Milliseconds()
		return outcome, err
	}

	downloadMatch, download := conditionArgument(condition, "download")
	if download && FrameReadCheckFromContext(ctx) == nil {
		return finish(m.waitForDownload(ctx, downloadMatch, timeout))
	}

	tabID, tabCtx, cancel, err := m.activeContextWithTimeout(ctx, timeout+2*time.Second)
	if err != nil {
		return WaitOutcome{Condition: condition}, err
	}
	defer cancel()
	currentTabID, currentTabCtx = tabID, tabCtx
	if FrameReadCheckFromContext(tabCtx) != nil {
		if err := m.guardCurrentURL(tabID, tabCtx); err != nil {
			return WaitOutcome{}, err
		}
	}
	if download {
		return finish(m.waitForDownload(ctx, downloadMatch, timeout))
	}

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

func conditionArgument(condition, name string) (string, bool) {
	if condition == name {
		return "", true
	}
	if rest, ok := strings.CutPrefix(condition, name+":"); ok {
		return rest, true
	}
	return "", false
}

func isReadinessCondition(condition string) bool {
	switch condition {
	case "load", "ready", "page_ready", "":
		return true
	}
	return false
}

func (m *Manager) waitForLoadEvent(ctx, tabCtx context.Context, tabID, condition string, timeout time.Duration) (WaitOutcome, error, bool) {
	observed, loaded := m.events.loadState(tabID)
	if !observed {
		return WaitOutcome{}, nil, false
	}
	if loaded {
		return WaitOutcome{ResolvedBy: WaitResolvedByEvent}, nil, true
	}

	if condition != "load" {
		return WaitOutcome{}, nil, false
	}

	sub, release := m.events.subscribe([]pageEventKind{eventLoad}, tabID)
	defer release()

	if _, loaded := m.events.loadState(tabID); loaded {
		return WaitOutcome{ResolvedBy: WaitResolvedByEvent}, nil, true
	}

	outcome, err := awaitEvent(ctx, tabCtx, sub, timeout, condition, func(ev pageEvent) bool {
		return ev.Kind == eventLoad
	})
	return outcome, err, true
}

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

func (m *Manager) waitForConditionScript(ctx context.Context, tabCtx context.Context, condition string, timeout time.Duration) (WaitOutcome, error) {
	deadline := time.Now().Add(timeout)
	attempts := 0
	for {

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

	sub, release := m.events.subscribe([]pageEventKind{eventDownload}, browserEventScope)
	defer release()

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
		var candidate DownloadEntry
		m.downloadsMu.Lock()
		for _, entry := range m.downloads {
			if settled[entry.GUID] || !matches(entry) {
				continue
			}
			if entry.State == string(downloadStateCompleted) {
				candidate = entry
				break
			}
			if entry.State == string(downloadStateCanceled) {
				candidate = entry
			}
		}
		m.downloadsMu.Unlock()
		if candidate.State != "" {
			if err := CheckDownloadSource(ctx, candidate.URL); err != nil {
				return WaitOutcome{}, err
			}
			if candidate.State == string(downloadStateCompleted) {
				return WaitOutcome{ResolvedBy: WaitResolvedByEvent, Wakeups: wakeups}, nil
			}
			if candidate.SuggestedFilename != "" {
				return WaitOutcome{Wakeups: wakeups}, fmt.Errorf("download %q was cancelled by the browser before it completed", candidate.SuggestedFilename)
			}
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
