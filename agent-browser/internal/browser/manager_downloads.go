package browser

import (
	"context"
	"os"
	"path/filepath"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

// maxTrackedDownloads bounds the in-memory download buffer so a long-lived
// session that triggers many downloads cannot grow it without limit. The
// oldest terminal entries are evicted first when the cap is exceeded.
const maxTrackedDownloads = 200

// Download state values, mirroring CDP's Browser.DownloadProgressState. Kept as
// local aliases so the rest of the codebase (and tests) does not depend on the
// cdproto symbol directly.
const (
	downloadStateInProgress = browser.DownloadProgressStateInProgress
	downloadStateCompleted  = browser.DownloadProgressStateCompleted
	downloadStateCanceled   = browser.DownloadProgressStateCanceled
)

// DownloadEntry is a single tracked download, populated from the
// Browser.downloadWillBegin and Browser.downloadProgress CDP events.
type DownloadEntry struct {
	GUID              string `json:"guid"`
	URL               string `json:"url"`
	SuggestedFilename string `json:"suggested_filename"`
	State             string `json:"state"` // inProgress | completed | canceled
	ReceivedBytes     int64  `json:"received_bytes"`
	TotalBytes        int64  `json:"total_bytes"`
	Path              string `json:"path,omitempty"`
}

// DownloadsResult is the drained snapshot returned by browser_downloads.
type DownloadsResult struct {
	Downloads []DownloadEntry `json:"downloads"`
	Count     int             `json:"count"`
	Note      string          `json:"note,omitempty"`
}

// ensureDownloadTracking is idempotent: on first call it picks a download
// directory under the user-data dir (or a temp dir when running against a
// remote endpoint), enables Browser.setDownloadBehavior with download events,
// and registers listeners that record download lifecycle into the bounded
// buffer. Subsequent calls are no-ops.
//
// With Chrome's flat session protocol the Browser.downloadWillBegin /
// downloadProgress events are delivered to the *target* (page) session, so we
// register the handler via ListenTarget on every known tab context as well as
// ListenBrowser on the root browser context as a fallback. New tab contexts
// created later pick up the listener in tabContext().
func (m *Manager) ensureDownloadTracking(ctx context.Context) error {
	m.downloadsMu.Lock()
	if m.downloadsEnabled {
		m.downloadsMu.Unlock()
		return nil
	}
	dir, err := m.resolveDownloadDir()
	if err != nil {
		m.downloadsMu.Unlock()
		return err
	}
	m.downloadDir = dir
	m.downloadsEnabled = true
	m.downloadsMu.Unlock()

	// Browser-level fallback listener.
	chromedp.ListenBrowser(m.browserCtx, m.handleDownloadEvent)

	// Make sure the active tab has a live context, then attach a target-level
	// listener to it (and to any other contexts already open). This is where
	// the download events actually arrive for page-initiated downloads.
	if _, err := m.ensureActive(ctx); err == nil {
		if tabID := m.refs.Active(); tabID != "" {
			if _, terr := m.tabContext(tabID); terr != nil {
				// Non-fatal: the browser-level fallback may still observe it.
				_ = terr
			}
		}
	}
	m.attachDownloadListenersToOpenTabs()

	// Browser.setDownloadBehavior is a browser-domain command; run it against
	// the browser executor like connect() does. allowAndName names completed
	// files by their download guid, which is generic and site-independent.
	return m.runBrowser(ctx, func(runCtx context.Context) error {
		return browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).
			WithDownloadPath(m.downloadDir).
			WithEventsEnabled(true).
			Do(runCtx)
	})
}

// handleDownloadEvent is the shared listener body for both browser- and
// target-level event delivery.
func (m *Manager) handleDownloadEvent(ev any) {
	switch e := ev.(type) {
	case *browser.EventDownloadWillBegin:
		m.recordDownloadBegin(e)
	case *browser.EventDownloadProgress:
		m.recordDownloadProgress(e)
	}
}

// attachDownloadListenersToOpenTabs registers the target-level download
// listener on every currently-open tab context.
func (m *Manager) attachDownloadListenersToOpenTabs() {
	m.mu.Lock()
	ctxs := make([]context.Context, 0, len(m.tabContexts))
	for _, tc := range m.tabContexts {
		ctxs = append(ctxs, tc.ctx)
	}
	m.mu.Unlock()
	for _, c := range ctxs {
		chromedp.ListenTarget(c, m.handleDownloadEvent)
	}
}

// attachDownloadListenerIfEnabled wires the target-level download listener onto
// a freshly created tab context when download tracking is already active.
// Called from tabContext under no lock.
func (m *Manager) attachDownloadListenerIfEnabled(tabCtx context.Context) {
	m.downloadsMu.Lock()
	enabled := m.downloadsEnabled
	m.downloadsMu.Unlock()
	if enabled {
		chromedp.ListenTarget(tabCtx, m.handleDownloadEvent)
	}
}

func (m *Manager) resolveDownloadDir() (string, error) {
	base := m.userDataDir
	var dir string
	if base != "" {
		dir = filepath.Join(base, "agent-browser-downloads")
	} else {
		// Remote-endpoint case: no local user-data dir is known to us.
		dir = filepath.Join(os.TempDir(), "agent-browser-downloads")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func (m *Manager) recordDownloadBegin(e *browser.EventDownloadWillBegin) {
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	if idx, ok := m.downloadIndex[e.GUID]; ok {
		entry := &m.downloads[idx]
		if e.URL != "" {
			entry.URL = e.URL
		}
		if e.SuggestedFilename != "" {
			entry.SuggestedFilename = e.SuggestedFilename
		}
		return
	}
	m.downloads = append(m.downloads, DownloadEntry{
		GUID:              e.GUID,
		URL:               e.URL,
		SuggestedFilename: e.SuggestedFilename,
		State:             string(downloadStateInProgress),
	})
	m.downloadIndex[e.GUID] = len(m.downloads) - 1
	m.trimDownloadsLocked()
}

func (m *Manager) recordDownloadProgress(e *browser.EventDownloadProgress) {
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	idx, ok := m.downloadIndex[e.GUID]
	if !ok {
		// Progress can arrive for a download whose begin event we missed
		// (e.g. listener wired mid-flight); synthesize an entry so it is not lost.
		m.downloads = append(m.downloads, DownloadEntry{GUID: e.GUID})
		idx = len(m.downloads) - 1
		m.downloadIndex[e.GUID] = idx
		m.trimDownloadsLocked()
		idx = m.downloadIndex[e.GUID]
	}
	entry := &m.downloads[idx]
	entry.ReceivedBytes = int64(e.ReceivedBytes)
	entry.TotalBytes = int64(e.TotalBytes)
	if e.State != "" {
		entry.State = string(e.State)
	}
	if e.FilePath != "" {
		entry.Path = e.FilePath
	}
	// On completion, fill in a best-effort path under the download dir when the
	// platform did not provide one (CDP names completed files by their guid).
	if e.State == downloadStateCompleted && entry.Path == "" && m.downloadDir != "" {
		entry.Path = filepath.Join(m.downloadDir, e.GUID)
	}
}

// trimDownloadsLocked evicts the oldest terminal entries once the buffer
// exceeds the cap. Caller must hold downloadsMu.
func (m *Manager) trimDownloadsLocked() {
	if len(m.downloads) <= maxTrackedDownloads {
		return
	}
	overflow := len(m.downloads) - maxTrackedDownloads
	m.downloads = m.downloads[overflow:]
	// Rebuild the index after a slice shift.
	m.downloadIndex = make(map[string]int, len(m.downloads))
	for i := range m.downloads {
		m.downloadIndex[m.downloads[i].GUID] = i
	}
}

// Downloads returns and drains the tracked downloads, modeled on the
// console-drain pattern. Enabling download tracking is lazy and idempotent so
// callers that never download pay nothing.
func (m *Manager) Downloads(ctx context.Context) (DownloadsResult, error) {
	if err := m.ensureDownloadTracking(ctx); err != nil {
		return DownloadsResult{}, err
	}
	m.downloadsMu.Lock()
	drained := m.downloads
	m.downloads = nil
	m.downloadIndex = map[string]int{}
	m.downloadsMu.Unlock()
	if drained == nil {
		drained = []DownloadEntry{}
	}
	return DownloadsResult{Downloads: drained, Count: len(drained)}, nil
}
