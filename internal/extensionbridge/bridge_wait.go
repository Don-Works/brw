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

// The extension transport has no chrome.debugger attachment, so it cannot
// subscribe to Browser.downloadProgress or Page.javascriptDialogOpening the way
// the direct-CDP transport does. Those two conditions are answered by re-asking
// the extension's own registries on a bounded, backing-off cadence. Everything
// else is answered by ONE awaited in-page promise, which is not a poll.
//
// The cadence starts tight enough that a wait feels immediate and backs off so a
// long wait does not keep the message port busy; each tick is one RPC over the
// bridge, which is why the ceiling matters more here than it would over CDP.
const (
	waitFallbackPollStart = 60 * time.Millisecond
	waitFallbackPollMax   = 400 * time.Millisecond
	// waitChunkGrace is how long past its own in-page timer one awaited chunk
	// may take to answer before the wait gives up on that round trip.
	waitChunkGrace = 2 * time.Second
)

// The NAMED capability failures for the two waits the extension answers from its
// own registries. A wait cannot degrade to the Supported=false note the snapshot
// calls return: it either resolves or says why it never can, so the caller does
// not read a timeout as "the download failed".
const (
	downloadWaitUnsupportedErr = "cannot wait for a download: the connected brw extension predates chrome.downloads support (issue #6) — reload the brw extension, or restart brw with the direct-CDP backend"
	dialogWaitUnsupportedErr   = "cannot wait for a dialog: the connected brw extension predates brw_dialog support — reload the brw extension, or restart brw with the direct-CDP backend"
)

// WaitFor blocks until condition holds.
func (b *Bridge) WaitFor(ctx context.Context, condition string, timeout time.Duration) error {
	// The cross-origin refusal lives in WaitForOutcome, which this delegates to.
	_, err := b.WaitForOutcome(ctx, condition, timeout)
	return err
}

// WaitForOutcome resolves a wait and reports what resolved it. It implements
// browser.WaitObserver, so brw_wait_for answers the same shape on both
// transports and a caller can see which mechanism it got.
func (b *Bridge) WaitForOutcome(ctx context.Context, condition string, timeout time.Duration) (browser.WaitOutcome, error) {
	if err := browser.GuardCrossOriginRefs("wait for", browser.BridgeCrossOriginRemedy, snapshot.WaitConditionRef(condition)); err != nil {
		return browser.WaitOutcome{}, err
	}
	if timeout == 0 {
		timeout = b.timeout
	}
	started := time.Now()
	finish := func(outcome browser.WaitOutcome, err error) (browser.WaitOutcome, error) {
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

// waitForConditionInPage waits via a SINGLE in-page promise (WaitConditionScript)
// that resolves the instant the condition holds — a MutationObserver/history-driven
// check running inside the renderer — instead of re-evaluating a heavy condition
// script across the bridge every 25-250ms. The old cross-process poll made each tick
// a full document.body.innerText / shadow-DOM walk; ten concurrent waits against a
// large (10k-row) page flooded the extension's debugger with hundreds of heavy
// evaluates a second and wedged the whole bridge until the waits expired. One
// awaited in-page promise per wait keeps bridge load flat no matter how many waits
// run concurrently. The await is chunked under b.timeout and re-armed, so a
// navigation that destroys the execution context simply continues against the new
// document.
func (b *Bridge) waitForConditionInPage(ctx context.Context, condition string, timeout time.Duration, deadline time.Time) (browser.WaitOutcome, error) {
	attempts := 0
	for {
		// Cooperative cancellation: a Cancel on the surrounding plan/batch (or this
		// tab) cancels ctx, unblocking a long wait promptly.
		if err := ctx.Err(); err != nil {
			return browser.WaitOutcome{Wakeups: attempts}, fmt.Errorf("wait for %q cancelled", condition)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return browser.WaitOutcome{Wakeups: attempts}, fmt.Errorf("timed out waiting for %q after %s; the condition was never met — check that the page is loaded and the condition is correct (valid: ready, committed, load, networkidle, text:..., url:..., title:..., ref:..., selector:..., fn:..., dialog, download, page_ready)", condition, timeout)
		}
		chunk := remaining
		if limit := b.waitChunkLimit(); chunk > limit {
			chunk = limit
		}
		attempts++
		// The in-page promise resolves at chunk on its own timer; the round trip
		// is bounded just past that so a renderer that never answers ends the
		// wait at the caller's timeout_ms rather than at the daemon's --timeout.
		chunkCtx, cancelChunk := context.WithTimeout(ctx, chunk+waitChunkGrace)
		matched, err := b.waitConditionOnce(chunkCtx, condition, chunk)
		cancelChunk()
		if err == nil && matched {
			return browser.WaitOutcome{ResolvedBy: browser.WaitResolvedByScript, Wakeups: attempts}, nil
		}
		if err != nil {
			// A navigation can destroy the in-page execution context mid-await; pause
			// briefly, then re-arm the promise against the new document rather than
			// hot-looping on the transient "context was destroyed" error.
			select {
			case <-ctx.Done():
				return browser.WaitOutcome{Wakeups: attempts}, fmt.Errorf("wait for %q cancelled", condition)
			case <-time.After(waitForErrBackoff):
			}
		}
	}
}

// waitForDownloadPolled re-reads the extension's chrome.downloads registry until
// a download that was not already finished when the wait started completes.
// Baseline and matching mirror the direct-CDP wait (see Manager.waitForDownload)
// down to the recency window, so the same wait answers the same way on both
// transports; only the cost of noticing differs.
func (b *Bridge) waitForDownloadPolled(ctx context.Context, match string, deadline time.Time) (browser.WaitOutcome, error) {
	needle := strings.ToLower(strings.TrimSpace(match))
	matches := func(entry browser.DownloadEntry) bool {
		if needle == "" {
			return true
		}
		return strings.Contains(strings.ToLower(entry.SuggestedFilename), needle) ||
			strings.Contains(strings.ToLower(entry.URL), needle)
	}

	// Baseline: a download that was already terminal when the wait started is old
	// news, UNLESS it reached that state inside the recency window — a wait is
	// written after the click that triggers it and a small file frequently
	// finishes first. This is the direct-CDP rule (Manager.downloadSettledBefore)
	// applied to the extension's own change timestamps; a build that sends none
	// gives every already-terminal download the "old" reading, which is what a
	// registry with no timestamp means on either transport.
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

// settledInsideWindow reports whether the extension recorded this download
// changing state at or after cutoff. An unknown time is old: it means the
// extension never told us when, not that it just happened.
func settledInsideWindow(changedAt map[string]time.Time, guid string, cutoff time.Time) bool {
	at, known := changedAt[guid]
	return known && !at.Before(cutoff)
}

// waitForDialogPolled re-reads the extension's answered-dialog ring. The read is
// a peek: consuming the ring here would steal the record from the brw_dialog
// status call the caller is very likely to make next.
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

// pollUntil runs check on a backing-off cadence until it reports true, fails, or
// the deadline passes. Wakeups counts the checks, which is what makes the cost
// of the fallback visible to the caller in the wait's own result.
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
