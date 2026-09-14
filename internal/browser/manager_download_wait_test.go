package browser

import (
	"context"
	"strings"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
)

// seedDownload puts an entry straight into the registry so the wait's semantics
// can be tested without racing a real browser download.
func seedDownload(m *Manager, guid, filename, state string, changedAt time.Time) {
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	m.ensureDownloadMapsLocked()
	m.downloads = append(m.downloads, DownloadEntry{
		GUID:              guid,
		URL:               "https://example.test/" + filename,
		SuggestedFilename: filename,
		State:             state,
	})
	m.rebuildDownloadIndexLocked()
	m.downloadSequence++
	m.downloadVersions[guid] = m.downloadSequence
	m.downloadChangedAt[guid] = changedAt
	// Tracking is already considered armed, so the wait does not try to talk to a
	// browser that these table cases do not need.
	m.downloadsEnabled = true
}

// A wait is written AFTER the click that triggers the download, so a small file
// routinely finishes before the wait runs. Treating every already-terminal
// download as old news made the wait hang for its full timeout on exactly the
// fast downloads it should answer instantly; treating none of them as old would
// let an unrelated download from earlier satisfy it straight away.
func TestWaitForDownloadRecencyWindow(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		changedAt time.Duration // age at the moment the wait starts
		match     string
		wantErr   string
	}{
		{
			name:      "a download that just completed satisfies the wait",
			state:     string(downloadStateCompleted),
			changedAt: 200 * time.Millisecond,
		},
		{
			name:      "a stale completed download does not",
			state:     string(downloadStateCompleted),
			changedAt: RecentDownloadWindow + time.Minute,
			wantErr:   "timed out",
		},
		{
			name:      "a recent cancellation is reported, not silently waited out",
			state:     string(downloadStateCanceled),
			changedAt: 200 * time.Millisecond,
			wantErr:   "cancelled by the browser",
		},
		{
			name:      "a matching filename satisfies a filtered wait",
			state:     string(downloadStateCompleted),
			changedAt: 200 * time.Millisecond,
			match:     "report",
		},
		{
			name:      "a non-matching filename does not",
			state:     string(downloadStateCompleted),
			changedAt: 200 * time.Millisecond,
			match:     "invoice",
			wantErr:   "timed out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{}
			seedDownload(m, "g1", "report.pdf", tt.state, time.Now().Add(-tt.changedAt))

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := m.waitForDownload(ctx, tt.match, 900*time.Millisecond)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("wait should have been satisfied, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("wait should have failed with %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// An in-progress download is unambiguous: wait for it however old it is. The
// completion is delivered the way Chrome delivers it — a Browser.downloadProgress
// event through the download subscription — so this also covers the ordering the
// wait depends on: the registry is written before waiters are woken.
func TestWaitForDownloadWaitsOutAnInProgressDownload(t *testing.T) {
	m := &Manager{}
	seedDownload(m, "g1", "big.iso", string(downloadStateInProgress), time.Now().Add(-time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := m.waitForDownload(ctx, "", 5*time.Second)
		done <- err
	}()

	// Still running: the wait must not return yet.
	select {
	case err := <-done:
		t.Fatalf("wait returned early while the download was still in progress: %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	m.handleDownloadEventForTab("", &cdpbrowser.EventDownloadProgress{
		GUID:  "g1",
		State: cdpbrowser.DownloadProgressStateCompleted,
	})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait should succeed once the download completes: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not notice the download completing")
	}
}

// Cancellation must be honoured promptly rather than running out the timeout.
func TestWaitForDownloadHonoursContextCancellation(t *testing.T) {
	m := &Manager{}
	seedDownload(m, "g1", "slow.bin", string(downloadStateInProgress), time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.waitForDownload(ctx, "", 30*time.Second)
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("err = %v, want a cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait ignored context cancellation")
	}
}

// downloadCompletedEvent is the CDP event Chrome sends when a download finishes.
func downloadCompletedEvent(guid string) *cdpbrowser.EventDownloadProgress {
	return &cdpbrowser.EventDownloadProgress{GUID: guid, State: cdpbrowser.DownloadProgressStateCompleted}
}

// TestDownloadWaitWakesOnlyOnEvents is the poll guard. A download wait used to
// re-read the registry every 50 ms, so a wait that sat idle for a second burned
// ~20 checks before the one that mattered. Reading the shared subscription
// instead means the wait wakes for events that actually happened and nothing
// else — restore the ticker and the wake count climbs with the idle time.
func TestDownloadWaitWakesOnlyOnEvents(t *testing.T) {
	m := &Manager{}
	seedDownload(m, "g1", "ledger.csv", string(downloadStateInProgress), time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type result struct {
		outcome WaitOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := m.waitForDownload(ctx, "", 5*time.Second)
		done <- result{outcome, err}
	}()

	// Sit idle far longer than the old 50 ms poll interval, then deliver exactly
	// two progress events: one that does not finish the download and one that does.
	time.Sleep(600 * time.Millisecond)
	m.handleDownloadEventForTab("", &cdpbrowser.EventDownloadProgress{
		GUID: "g1", State: cdpbrowser.DownloadProgressStateInProgress, ReceivedBytes: 128,
	})
	m.handleDownloadEventForTab("", &cdpbrowser.EventDownloadProgress{
		GUID: "g1", State: cdpbrowser.DownloadProgressStateCompleted,
	})

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("wait failed: %v", got.err)
		}
		if got.outcome.ResolvedBy != WaitResolvedByEvent {
			t.Fatalf("resolved_by = %q, want %q", got.outcome.ResolvedBy, WaitResolvedByEvent)
		}
		// Two delivered events is the ceiling; a 50 ms poll over the same idle
		// window would have woken more than ten times.
		if got.outcome.Wakeups > 2 {
			t.Fatalf("wait woke %d times for 2 delivered events — something is polling", got.outcome.Wakeups)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not resolve from the download events")
	}
}
