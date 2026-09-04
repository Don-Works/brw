package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

// maxTrackedDownloads bounds the in-memory download buffer so a long-lived
// session that triggers many downloads cannot grow it without limit. The
// oldest terminal entries are evicted first when the cap is exceeded.
const maxTrackedDownloads = 200

const (
	maxDownloadGUIDBytes     = 500
	maxDownloadURLBytes      = 8 << 10
	maxDownloadFilenameBytes = 1000
)

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
	TabID             string `json:"tab_id,omitempty"`
	State             string `json:"state"` // inProgress | completed | canceled
	ReceivedBytes     int64  `json:"received_bytes"`
	TotalBytes        int64  `json:"total_bytes"`
	Path              string `json:"path,omitempty"`
}

// DownloadsResult is the bounded snapshot returned by brw_downloads.
type DownloadsResult struct {
	Downloads []DownloadEntry `json:"downloads"`
	Count     int             `json:"count"`
	Note      string          `json:"note,omitempty"`
	// Supported reports whether the active backend can capture downloads. The
	// direct-CDP Manager sets this true; the extension bridge sets it false (it
	// cannot observe download events without extension-side chrome.downloads
	// support — see Bridge.Downloads). Clients can branch on this flag instead
	// of pattern-matching the human-readable Note.
	Supported bool `json:"supported"`
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
	// Mark as enabled before releasing the lock so concurrent callers block on
	// downloadsMu and see the flag as true. The actual setup runs below; if it
	// fails, we clear the flag so a later retry can succeed.
	dir, err := m.resolveDownloadDir()
	if err != nil {
		m.downloadsMu.Unlock()
		return err
	}
	m.downloadDir = dir
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
	if err := m.runBrowser(ctx, func(runCtx context.Context) error {
		return browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).
			WithDownloadPath(m.downloadDir).
			WithEventsEnabled(true).
			Do(runCtx)
	}); err != nil {
		// Setup failed — clear the flag so a later retry can succeed.
		m.downloadsMu.Lock()
		m.downloadsEnabled = false
		m.downloadsMu.Unlock()
		_ = m.cleanupDownloadStaging()
		return err
	}
	m.downloadsMu.Lock()
	m.downloadsEnabled = true
	m.downloadsMu.Unlock()
	return nil
}

// handleDownloadEvent is the shared listener body for both browser- and
// target-level event delivery.
func (m *Manager) handleDownloadEvent(ev any) {
	m.handleDownloadEventForTab("", ev)
}

// handleDownloadEventForTab retains the target that delivered an event. CDP's
// progress event does not carry a frame/target id, so provenance has to come
// from the per-target listener closure rather than from the event payload.
func (m *Manager) handleDownloadEventForTab(tabID string, ev any) {
	switch e := ev.(type) {
	case *browser.EventDownloadWillBegin:
		if tabID == "" {
			// Current Chromium emits Browser-domain download events on the browser
			// connection even when target listeners are installed. For a top-level
			// frame its frame id is the page target id; accept that attribution only
			// when it exactly names a context Manager already owns. Subframe ids are
			// left unattributed instead of guessing.
			frameID := string(e.FrameID)
			m.mu.RLock()
			_, known := m.tabContexts[frameID]
			m.mu.RUnlock()
			if known {
				tabID = frameID
			}
		}
		m.recordDownloadBeginForTab(tabID, e)
	case *browser.EventDownloadProgress:
		m.recordDownloadProgressForTab(tabID, e)
	}
}

// attachDownloadListenersToOpenTabs registers the target-level download
// listener on every currently-open tab context.
func (m *Manager) attachDownloadListenersToOpenTabs() {
	m.mu.RLock()
	tabs := make([]struct {
		id  string
		ctx context.Context
	}, 0, len(m.tabContexts))
	for id, tc := range m.tabContexts {
		tabs = append(tabs, struct {
			id  string
			ctx context.Context
		}{id: id, ctx: tc.ctx})
	}
	m.mu.RUnlock()
	for _, tab := range tabs {
		tab := tab
		chromedp.ListenTarget(tab.ctx, func(ev any) {
			m.handleDownloadEventForTab(tab.id, ev)
		})
	}
}

// attachDownloadListenerIfEnabled wires the target-level download listener onto
// a freshly created tab context when download tracking is already active.
// Called from tabContext under no lock.
func (m *Manager) attachDownloadListenerIfEnabled(tabID string, tabCtx context.Context) {
	m.downloadsMu.Lock()
	enabled := m.downloadsEnabled
	m.downloadsMu.Unlock()
	if enabled {
		chromedp.ListenTarget(tabCtx, func(ev any) {
			m.handleDownloadEventForTab(tabID, ev)
		})
	}
}

func (m *Manager) resolveDownloadDir() (string, error) {
	base := strings.TrimSpace(m.userDataDir)
	if base == "" {
		// Remote-endpoint case: use the private per-user cache rather than a
		// shared, predictable /tmp directory.
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve download cache: %w", err)
		}
		base = filepath.Join(cache, "brw")
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("browser download staging base must be absolute")
	}
	downloadsBase := filepath.Join(base, "brw-downloads")
	if info, err := os.Lstat(downloadsBase); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("browser download staging directory must not be a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := rejectDownloadGitCheckout(downloadsBase); err != nil {
		return "", err
	}
	if err := os.MkdirAll(downloadsBase, 0o700); err != nil {
		return "", err
	}
	resolvedBase, err := filepath.EvalSymlinks(downloadsBase)
	if err != nil {
		return "", err
	}
	if err := rejectDownloadGitCheckout(resolvedBase); err != nil {
		return "", err
	}
	info, err := os.Stat(resolvedBase)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("browser download staging path is not a directory")
	}
	if err := os.Chmod(resolvedBase, 0o700); err != nil {
		return "", err
	}
	// A unique owned session directory prevents two profiles/daemons from
	// observing or deleting one another's staged files. Manager.Close removes it.
	dir, err := os.MkdirTemp(resolvedBase, "session-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	m.downloadDirOwned = true
	return dir, nil
}

func (m *Manager) recordDownloadBegin(e *browser.EventDownloadWillBegin) {
	m.recordDownloadBeginForTab("", e)
}

func (m *Manager) recordDownloadBeginForTab(tabID string, e *browser.EventDownloadWillBegin) {
	if e == nil || !validDownloadGUID(e.GUID) {
		return
	}
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	m.ensureDownloadMapsLocked()
	if idx, ok := m.downloadIndex[e.GUID]; ok {
		entry := &m.downloads[idx]
		if e.URL != "" {
			entry.URL = boundedDownloadString(e.URL, maxDownloadURLBytes)
		}
		if e.SuggestedFilename != "" {
			entry.SuggestedFilename = boundedDownloadString(e.SuggestedFilename, maxDownloadFilenameBytes)
		}
		if tabID != "" && entry.TabID == "" {
			entry.TabID = tabID
		}
		m.markDownloadChangedLocked(e.GUID)
		return
	}
	m.downloads = append(m.downloads, DownloadEntry{
		GUID:              e.GUID,
		URL:               boundedDownloadString(e.URL, maxDownloadURLBytes),
		SuggestedFilename: boundedDownloadString(e.SuggestedFilename, maxDownloadFilenameBytes),
		TabID:             tabID,
		State:             string(downloadStateInProgress),
	})
	m.downloadIndex[e.GUID] = len(m.downloads) - 1
	m.markDownloadChangedLocked(e.GUID)
	m.trimDownloadsLocked()
}

func (m *Manager) recordDownloadProgressForTab(tabID string, e *browser.EventDownloadProgress) {
	if e == nil || !validDownloadGUID(e.GUID) {
		return
	}
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	m.ensureDownloadMapsLocked()
	idx, ok := m.downloadIndex[e.GUID]
	if !ok {
		// Progress can arrive for a download whose begin event we missed
		// (e.g. listener wired mid-flight); synthesize an entry so it is not lost.
		m.downloads = append(m.downloads, DownloadEntry{GUID: e.GUID, TabID: tabID})
		idx = len(m.downloads) - 1
		m.downloadIndex[e.GUID] = idx
	}
	entry := &m.downloads[idx]
	if tabID != "" && entry.TabID == "" {
		entry.TabID = tabID
	}
	entry.ReceivedBytes = int64(e.ReceivedBytes)
	entry.TotalBytes = int64(e.TotalBytes)
	if e.State != "" {
		entry.State = string(e.State)
	}
	// Never accept an arbitrary browser-supplied host path. allowAndName writes
	// the file under our private staging directory using its validated GUID, so
	// derive the only path Artifact Service is allowed to open.
	if e.State == downloadStateCompleted && m.downloadDir != "" {
		entry.Path = filepath.Join(m.downloadDir, e.GUID)
	}
	m.markDownloadChangedLocked(e.GUID)
	m.trimDownloadsLocked()
}

// trimDownloadsLocked evicts the oldest terminal entries once the buffer
// exceeds the cap. Caller must hold downloadsMu.
func (m *Manager) trimDownloadsLocked() {
	for len(m.downloads) > maxTrackedDownloads {
		remove := -1
		for index := range m.downloads {
			if m.downloads[index].State == string(downloadStateCompleted) || m.downloads[index].State == string(downloadStateCanceled) {
				remove = index
				break
			}
		}
		if remove < 0 {
			remove = 0
		}
		guid := m.downloads[remove].GUID
		m.downloads = append(m.downloads[:remove], m.downloads[remove+1:]...)
		delete(m.downloadIndex, guid)
		delete(m.downloadVersions, guid)
	}
	m.rebuildDownloadIndexLocked()
}

func (m *Manager) ensureDownloadMapsLocked() {
	if m.downloadIndex == nil {
		m.downloadIndex = map[string]int{}
	}
	if m.downloadVersions == nil {
		m.downloadVersions = map[string]uint64{}
	}
	if m.downloadCursors == nil {
		m.downloadCursors = map[string]uint64{}
	}
}

func (m *Manager) markDownloadChangedLocked(guid string) {
	m.downloadSequence++
	m.downloadVersions[guid] = m.downloadSequence
}

func (m *Manager) rebuildDownloadIndexLocked() {
	m.downloadIndex = make(map[string]int, len(m.downloads))
	for i := range m.downloads {
		m.downloadIndex[m.downloads[i].GUID] = i
	}
}

// Downloads returns a non-draining bounded snapshot. Recipe-scoped contexts
// receive only entries changed since that tab's prior call: ArmEvent's first
// call establishes a baseline without deleting an in-progress entry, and later
// progress updates still retain the begin event's filename and provenance.
func (m *Manager) Downloads(ctx context.Context) (DownloadsResult, error) {
	if err := m.ensureDownloadTracking(ctx); err != nil {
		return DownloadsResult{}, err
	}
	m.downloadsMu.Lock()
	m.ensureDownloadMapsLocked()
	result := make([]DownloadEntry, 0, len(m.downloads))
	if _, recipeScoped := AllowedOriginsFromContext(ctx); recipeScoped {
		tabID := TabIDFromContext(ctx)
		cursor := m.downloadCursors[tabID]
		for _, entry := range m.downloads {
			// A deterministic recipe must never accept an unattributed event from
			// another open tab. Ordinary manual snapshots still expose unknown
			// provenance so callers can inspect legacy/backend-limited events.
			if tabID != "" && entry.TabID != tabID {
				continue
			}
			if m.downloadVersions[entry.GUID] > cursor {
				result = append(result, entry)
			}
		}
		m.downloadCursors[tabID] = m.downloadSequence
	} else {
		result = append(result, m.downloads...)
	}
	m.downloadsMu.Unlock()
	if result == nil {
		result = []DownloadEntry{}
	}
	return DownloadsResult{Downloads: result, Count: len(result), Supported: true}, nil
}

// CleanupManagedDownload removes only a file from this Manager's private,
// allowAndName staging directory. Artifact Service calls it after persistence;
// transports backed by a user's normal Downloads folder do not implement this
// capability and therefore retain their originals.
func (m *Manager) CleanupManagedDownload(item DownloadEntry) (bool, error) {
	m.downloadsMu.Lock()
	dir := m.downloadDir
	owned := m.downloadDirOwned
	m.downloadsMu.Unlock()
	if !owned || dir == "" || !validDownloadGUID(item.GUID) {
		return false, nil
	}
	path, err := filepath.Abs(item.Path)
	if err != nil {
		return false, err
	}
	if filepath.Clean(path) != filepath.Join(dir, item.GUID) {
		return false, nil
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	if resolvedParent != dir {
		return false, errors.New("managed download staging parent changed")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	m.downloadsMu.Lock()
	if index, ok := m.downloadIndex[item.GUID]; ok {
		m.downloads = append(m.downloads[:index], m.downloads[index+1:]...)
		delete(m.downloadVersions, item.GUID)
		m.rebuildDownloadIndexLocked()
	}
	m.downloadsMu.Unlock()
	return true, nil
}

func validDownloadGUID(value string) bool {
	if value == "" || len(value) > maxDownloadGUIDBytes {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func boundedDownloadString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (m *Manager) cleanupDownloadStaging() error {
	m.downloadsMu.Lock()
	dir := m.downloadDir
	owned := m.downloadDirOwned
	m.downloadDir = ""
	m.downloadDirOwned = false
	m.downloadsMu.Unlock()
	if !owned || dir == "" {
		return nil
	}
	if !strings.HasPrefix(filepath.Base(dir), "session-") {
		return errors.New("refusing to remove unrecognized browser download staging directory")
	}
	return os.RemoveAll(dir)
}

func rejectDownloadGitCheckout(path string) error {
	probe := filepath.Clean(path)
	for {
		if _, err := os.Stat(filepath.Join(probe, ".git")); err == nil {
			return errors.New("browser download staging directory must not be inside a Git checkout")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return nil
		}
		probe = parent
	}
}
