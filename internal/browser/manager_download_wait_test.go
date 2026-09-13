package browser

import (
	"context"
	"strings"
	"testing"
	"time"
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
			changedAt: recentDownloadWindow + time.Minute,
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
			err := m.waitForDownload(ctx, tt.match, 900*time.Millisecond)

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

// An in-progress download is unambiguous: wait for it however old it is.
func TestWaitForDownloadWaitsOutAnInProgressDownload(t *testing.T) {
	m := &Manager{}
	seedDownload(m, "g1", "big.iso", string(downloadStateInProgress), time.Now().Add(-time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.waitForDownload(ctx, "", 5*time.Second) }()

	// Still running: the wait must not return yet.
	select {
	case err := <-done:
		t.Fatalf("wait returned early while the download was still in progress: %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	m.downloadsMu.Lock()
	m.downloads[0].State = string(downloadStateCompleted)
	m.downloadChangedAt["g1"] = time.Now()
	m.downloadsMu.Unlock()

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
	go func() { done <- m.waitForDownload(ctx, "", 30*time.Second) }()
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
