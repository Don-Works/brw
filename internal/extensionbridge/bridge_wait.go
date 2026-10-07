package extensionbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

const (
	waitFallbackPollStart = 60 * time.Millisecond
	waitFallbackPollMax   = 400 * time.Millisecond

	waitChunkGrace = 2 * time.Second
)

const (
	downloadWaitUnsupportedErr = "cannot wait for a download: the connected brw extension predates chrome.downloads support (issue #6) — reload the brw extension, or restart brw with the direct-CDP backend"
	dialogWaitUnsupportedErr   = "cannot wait for a dialog: the connected brw extension predates brw_dialog support — reload the brw extension, or restart brw with the direct-CDP backend"
)

// WaitFor blocks until condition holds.
func (b *Bridge) WaitFor(ctx context.Context, condition string, timeout time.Duration) error {
	_, err := b.WaitForOutcome(ctx, condition, timeout)
	return err
}

// WaitForOutcome resolves a wait and reports what resolved it.
func (b *Bridge) WaitForOutcome(ctx context.Context, condition string, timeout time.Duration) (browser.WaitOutcome, error) {
	if err := browser.GuardCrossOriginRefs("wait for", browser.BridgeCrossOriginRemedy, snapshot.WaitConditionRef(condition)); err != nil {
		return browser.WaitOutcome{}, err
	}
	if timeout == 0 {
		timeout = b.timeout
	}
	started := time.Now()
	finish := func(outcome browser.WaitOutcome, err error) (browser.WaitOutcome, error) {
		if guardErr := b.guardCurrentURL(ctx); guardErr != nil {
			return browser.WaitOutcome{}, guardErr
		}
		outcome.Condition = condition
		outcome.OK = err == nil
		outcome.WaitedMS = time.Since(started).Milliseconds()
		return outcome, err
	}
	deadline := time.Now().Add(timeout)

	if match, ok := waitConditionArgument(condition, "download"); ok {
		return finish(b.waitForDownloadPolled(ctx, match, deadline))
	}
	if match, ok := waitConditionArgument(condition, "dialog"); ok {
		return finish(b.waitForDialogPolled(ctx, match, deadline))
	}
	return finish(b.waitForConditionInPage(ctx, condition, timeout, deadline))
}

func waitConditionArgument(condition, name string) (string, bool) {
	if condition == name {
		return "", true
	}
	if rest, ok := strings.CutPrefix(condition, name+":"); ok {
		return rest, true
	}
	return "", false
}

func (b *Bridge) waitForConditionInPage(ctx context.Context, condition string, timeout time.Duration, deadline time.Time) (browser.WaitOutcome, error) {
	waitCtx, cancelWait := context.WithDeadline(ctx, deadline)
	defer cancelWait()
	attempts := 0
	timedOut := func() (browser.WaitOutcome, error) {
		return browser.WaitOutcome{Wakeups: attempts}, fmt.Errorf("timed out waiting for %q after %s; the condition was never met — check that the page is loaded and the condition is correct (valid: ready, committed, load, networkidle, text:..., url:..., title:..., ref:..., selector:..., fn:..., dialog, download, page_ready)", condition, timeout)
	}
	for {
		if err := ctx.Err(); err != nil {
			return browser.WaitOutcome{Wakeups: attempts}, fmt.Errorf("wait for %q cancelled", condition)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || waitCtx.Err() != nil {
			return timedOut()
		}
		chunk := remaining
		if limit := b.waitChunkLimit(); chunk > limit {
			chunk = limit
		}
		attempts++

		chunkCtx, cancelChunk := context.WithTimeout(waitCtx, chunk+waitChunkGrace)
		matched, err := b.waitConditionOnce(chunkCtx, condition, chunk)
		cancelChunk()
		if ctx.Err() != nil {
			return browser.WaitOutcome{Wakeups: attempts}, fmt.Errorf("wait for %q cancelled", condition)
		}
		if err == nil && matched && time.Now().Before(deadline) {
			return browser.WaitOutcome{ResolvedBy: browser.WaitResolvedByScript, Wakeups: attempts}, nil
		}
		if time.Until(deadline) <= 0 || waitCtx.Err() != nil {
			return timedOut()
		}
		if err != nil {
			select {
			case <-waitCtx.Done():
				if ctx.Err() != nil {
					return browser.WaitOutcome{Wakeups: attempts}, fmt.Errorf("wait for %q cancelled", condition)
				}
				return timedOut()
			case <-time.After(waitForErrBackoff):
			}
		}
	}
}

func (b *Bridge) waitForDownloadPolled(ctx context.Context, match string, deadline time.Time) (browser.WaitOutcome, error) {
	needle := strings.ToLower(strings.TrimSpace(match))
	matches := func(entry browser.DownloadEntry) bool {
		if needle == "" {
			return true
		}
		return strings.Contains(strings.ToLower(entry.SuggestedFilename), needle) ||
			strings.Contains(strings.ToLower(entry.URL), needle)
	}

	baseline := map[string]bool{}
	first := true
	cutoff := time.Now().Add(-browser.RecentDownloadWindow)

	return b.pollUntil(ctx, deadline, `download`, func() (bool, error) {
		snapshot, err := b.downloadSnapshot(ctx)
		if err != nil {
			return false, err
		}
		if !snapshot.Supported {
			return false, errors.New(downloadWaitUnsupportedErr)
		}
		for _, entry := range snapshot.Downloads {
			terminal := entry.State == "completed" || entry.State == "canceled"
			if first && terminal && !settledInsideWindow(snapshot.ChangedAt, entry.GUID, cutoff) {
				baseline[entry.GUID] = true
				continue
			}
			if baseline[entry.GUID] || !matches(entry) {
				continue
			}
			if err := browser.CheckDownloadSource(ctx, entry.URL); err != nil {
				return false, err
			}
			if entry.State == "completed" {
				return true, nil
			}
			if entry.State == "canceled" {
				return false, fmt.Errorf("download %q was cancelled by the browser before it completed", entry.SuggestedFilename)
			}
		}
		first = false
		return false, nil
	})
}

func settledInsideWindow(changedAt map[string]time.Time, guid string, cutoff time.Time) bool {
	at, known := changedAt[guid]
	return known && !at.Before(cutoff)
}

func (b *Bridge) waitForDialogPolled(ctx context.Context, match string, deadline time.Time) (browser.WaitOutcome, error) {
	needle := strings.ToLower(strings.TrimSpace(match))
	cutoff := time.Now().Add(-browser.RecentDialogWindow)
	condition := "dialog"
	if needle != "" {
		condition = "dialog:" + match
	}

	return b.pollUntil(ctx, deadline, condition, func() (bool, error) {
		result, err := b.Dialog(ctx, browser.DialogOptions{Action: "status", Peek: true})
		if err != nil {
			return false, err
		}
		if !result.Supported {
			return false, errors.New(dialogWaitUnsupportedErr)
		}
		for _, record := range result.Dialogs {
			at, parseErr := time.Parse(time.RFC3339Nano, record.At)
			if parseErr == nil && at.Before(cutoff) {
				continue
			}
			if needle == "" ||
				strings.Contains(strings.ToLower(record.Message), needle) ||
				strings.Contains(strings.ToLower(record.Type), needle) {
				return true, nil
			}
		}
		return false, nil
	})
}

func (b *Bridge) pollUntil(ctx context.Context, deadline time.Time, condition string, check func() (bool, error)) (browser.WaitOutcome, error) {
	interval := waitFallbackPollStart
	checks := 0
	for {
		if err := ctx.Err(); err != nil {
			return browser.WaitOutcome{Wakeups: checks}, fmt.Errorf("wait for %q cancelled", condition)
		}
		checks++
		done, err := check()
		if err != nil {
			return browser.WaitOutcome{Wakeups: checks}, err
		}
		if done {
			return browser.WaitOutcome{ResolvedBy: browser.WaitResolvedByPoll, Wakeups: checks}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return browser.WaitOutcome{Wakeups: checks}, fmt.Errorf("timed out waiting for %q", condition)
		}
		if interval > remaining {
			interval = remaining
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return browser.WaitOutcome{Wakeups: checks}, fmt.Errorf("wait for %q cancelled", condition)
		case <-timer.C:
		}
		if interval *= 2; interval > waitFallbackPollMax {
			interval = waitFallbackPollMax
		}
	}
}
