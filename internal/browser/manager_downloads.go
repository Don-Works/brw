package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/browser"
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
	// FilePaths reports whether a completed entry carries a path brw may open.
	// It is a second axis on purpose: a lane that drives the browser its user is
	// signed into observes every download and reports none of their paths,
	// because brw refuses to route that browser's downloads through a directory
	// it owns. Digest assertions and download captures read this rather than
	// treating a missing path as a per-download failure.
	FilePaths bool `json:"file_paths"`
}

// ErrDownloadRoutingSignedIn refuses to decide where downloads land on a lane
// that drives the browser the user is personally signed into.
//
// Browser.setDownloadBehavior has no per-download and no per-tab scope: the
// narrowest thing it applies to is a whole browser context, and on this lane
// the default browser context is the user's own windows. Pointing it at brw's
// staging directory would silently send every file that person downloads by
// hand into a directory named by download GUID that Manager.Close then deletes.
// So the capability is refused rather than delivered by taking over their
// browser; downloads are still OBSERVED, they just land where Chrome was
// already sending them.
var ErrDownloadRoutingSignedIn = errors.New("brw will not choose where downloads land on a transport that drives the browser you are signed into: the DevTools command applies to the whole browser context, so it would redirect the files you download by hand into brw's staging directory. downloads are still reported by brw_downloads, at the browser's own destination — use a direct-CDP profile to stage them")

// stagesDownloads reports whether this lane may point the browser at a brw-owned
// download directory. It is the single place the refusal is decided, so no code
// path can reach Browser.setDownloadBehavior with a path by forgetting to ask.
func (m *Manager) stagesDownloads() bool {
	return !m.signedInProfile
}

// ensureDownloadTracking is idempotent: on first call it picks a download
// directory under the user-data dir (or a temp dir when running against a
// remote endpoint), enables Browser.setDownloadBehavior with download events,
// and registers listeners that record download lifecycle into the bounded
// buffer. Subsequent calls are no-ops.
//
// On a signed-in lane it stages nothing: the behaviour is left at Chrome's
// default and only the event stream is switched on, so brw observes the
// downloads the browser was going to make anyway and moves none of them.
//
// With Chrome's flat session protocol the Browser.downloadWillBegin /
// downloadProgress events are delivered to the *target* (page) session, so every
// tab context forwards them from its own subscription (see tabContext); this
// adds the browser-connection subscription as the fallback for a download no
// page session claims.
func (m *Manager) ensureDownloadTracking(ctx context.Context) error {
	m.downloadsMu.Lock()
	if m.downloadsEnabled {
		m.downloadsMu.Unlock()
		return nil
	}
	// Mark as enabled before releasing the lock so concurrent callers block on
	// downloadsMu and see the flag as true. The actual setup runs below; if it
	// fails, we clear the flag so a later retry can succeed.
	if m.stagesDownloads() {
		dir, err := m.resolveDownloadDir()
		if err != nil {
			m.downloadsMu.Unlock()
			return err
		}
		m.downloadDir = dir
	}
	m.downloadsMu.Unlock()

	// Browser-connection fallback subscription.
	m.events.attachBrowser(m.browserCtx, m.handleDownloadEvent)

	// Make sure the active tab has a live context: creating it installs the tab
	// subscription, which is where page-initiated download events actually
	// arrive.
	if _, err := m.ensureActive(ctx); err == nil {
		if tabID := m.refs.Active(); tabID != "" {
			if _, terr := m.tabContext(tabID); terr != nil {
				// Non-fatal: the browser-level fallback may still observe it.
				_ = terr
			}
		}
	}

	if err := m.applyDownloadBehavior(ctx, m.downloadDir); err != nil {
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
//
// It is the sole consumer of Browser.downloadProgress and the sole publisher of
// the download signal a wait subscribes to, which is what guarantees the order
// the wait depends on: the registry is written, THEN waiters are woken to
// re-read it. Waking first would hand a waiter the pre-change state with no
// further event coming to correct it.
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
		m.events.publish(browserEventScope, pageEvent{Kind: eventDownload, ID: e.GUID, URL: e.URL})
	case *browser.EventDownloadProgress:
		m.recordDownloadProgressForTab(tabID, e)
		m.events.publish(browserEventScope, pageEvent{Kind: eventDownload, ID: e.GUID, Detail: string(e.State)})
	}
}

// applyDownloadBehavior points Chrome at dir, or leaves the browser's own
// destination alone when dir is empty, and turns the download event stream on
// either way. Browser.setDownloadBehavior is a browser-domain command, so it
// runs against the browser executor.
//
// This is the ONLY place brw sends that command, and it refuses a directory on
// a signed-in lane. The command's narrowest scope is a whole browser context,
// so a path here would move the files the person using that browser downloads
// by hand; putting the refusal at the command rather than at each caller is
// what keeps a future caller from reaching it.
//
// "default" is Chrome's own behaviour and is also the value that undoes an
// override, which is why this lane leaves nothing to restore at Close.
func (m *Manager) applyDownloadBehavior(ctx context.Context, dir string) error {
	if dir != "" && !m.stagesDownloads() {
		return ErrDownloadRoutingSignedIn
	}
	return m.runBrowser(ctx, func(runCtx context.Context) error {
		if dir == "" {
			return browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorDefault).
				WithEventsEnabled(true).
				Do(runCtx)
		}
		return browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).
			WithDownloadPath(dir).
			WithEventsEnabled(true).
			Do(runCtx)
	})
}

func (m *Manager) resolveDownloadDir() (string, error) {
	base := strings.TrimSpace(m.userDataDir)
	// An attach-only lane's user data directory is the browser's own — with
	// --remote, whatever profile that browser was started against. Staging
	// downloads under it would have brw creating directories inside somebody
	// else's profile, so it falls through to the per-user cache below exactly as
	// an endpoint with no known directory does. A lane that also drives a
	// SIGNED-IN browser never reaches here at all: it stages nothing.
	if m.attachOnly {
		base = ""
	}
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
		delete(m.downloadChangedAt, guid)
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
	if m.downloadChangedAt == nil {
		m.downloadChangedAt = map[string]time.Time{}
	}
}

func (m *Manager) markDownloadChangedLocked(guid string) {
	m.downloadSequence++
	m.downloadVersions[guid] = m.downloadSequence
	if m.downloadChangedAt == nil {
		m.downloadChangedAt = map[string]time.Time{}
	}
	m.downloadChangedAt[guid] = time.Now()
}

// downloadSettledBefore reports whether a download reached its terminal state
// long enough ago to be treated as old news by a wait that starts now.
//
// brw_wait_for{condition:"download"} is written after the click that triggers
// the download, so by the time the wait runs a small file has often already
// finished. Ignoring every already-terminal download made the wait hang for its
// whole timeout on exactly the fast downloads it should have answered
// instantly. Ignoring none of them would let an unrelated download from earlier
// in the session satisfy the wait immediately, which is the opposite failure.
//
// A short recency window separates the two: a download that finished within it
// belongs to the action the caller just took, and anything older does not.
func (m *Manager) downloadSettledBefore(guid string, cutoff time.Time) bool {
	changed, known := m.downloadChangedAt[guid]
	if !known {
		// No timestamp means it predates this bookkeeping entirely, so it is old.
		return true
	}
	return changed.Before(cutoff)
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
	out := DownloadsResult{Downloads: result, Count: len(result), Supported: true, FilePaths: m.stagesDownloads()}
	if !out.FilePaths {
		out.Note = signedInDownloadNote
	}
	return out, nil
}

// signedInDownloadNote says why entries from this lane carry no path. It is on
// every snapshot rather than only the empty one: an agent that sees downloads
// listed and no path would otherwise read the gap as a bug.
const signedInDownloadNote = "downloads are observed but not staged on this transport: it drives the browser you are signed into, so brw leaves the destination alone and does not report a file path. brw_downloads still reports every download; a digest assertion or a download capture needs a direct-CDP profile"

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

// retireDownloadStaging steps off the managed staging directory without deleting
// it, so files already downloaded into it stay where brw_downloads says they
// are. cleanupDownloadStaging removes every retired directory at Close.
func (m *Manager) retireDownloadStaging() {
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	dir, owned := m.downloadDir, m.downloadDirOwned
	m.downloadDir = ""
	m.downloadDirOwned = false
	if !owned || dir == "" {
		return
	}
	// An empty staging directory holds no file brw_downloads ever named, so it
	// goes now rather than at Close. Toggling brw_set_download_path builds a
	// fresh one on every restore, and keeping each would leave one directory per
	// toggle on disk and one entry per toggle in this list until shutdown.
	// os.Remove is the emptiness test: it refuses a directory still holding
	// anything, which is exactly the directory that has to be kept.
	if strings.HasPrefix(filepath.Base(dir), "session-") && os.Remove(dir) == nil {
		return
	}
	m.retiredDownloadDirs = append(m.retiredDownloadDirs, dir)
}

func (m *Manager) cleanupDownloadStaging() error {
	m.downloadsMu.Lock()
	dirs := m.retiredDownloadDirs
	m.retiredDownloadDirs = nil
	if m.downloadDirOwned && m.downloadDir != "" {
		dirs = append(dirs, m.downloadDir)
	}
	m.downloadDir = ""
	m.downloadDirOwned = false
	m.downloadsMu.Unlock()
	var failures error
	for _, dir := range dirs {
		if !strings.HasPrefix(filepath.Base(dir), "session-") {
			failures = errors.Join(failures, errors.New("refusing to remove unrecognized browser download staging directory"))
			continue
		}
		failures = errors.Join(failures, os.RemoveAll(dir))
	}
	return failures
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
