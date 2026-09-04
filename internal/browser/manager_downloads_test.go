package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/store"
	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// newHeadlessManager builds a Manager wired to a headless Chrome launched via a
// chromedp ExecAllocator. It exercises the real download-tracking code paths
// (ensureDownloadTracking, the target-level listener, recordDownload*, and the
// Downloads snapshot) without depending on the production visible-Chrome launcher,
// which is slow/fragile under headless CI on this platform.
func newHeadlessManager(t *testing.T) *Manager {
	t.Helper()
	chromePath, err := cdp.FindChrome("")
	if err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("disable-gpu", true),
		chromedp.UserDataDir(t.TempDir()),
		chromedp.WSURLReadTimeout(45*time.Second),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		t.Skipf("headless Chrome did not start: %v", err)
	}
	m := &Manager{
		allocCancel:        allocCancel,
		browserCtx:         browserCtx,
		browserCancel:      browserCancel,
		tabContexts:        map[string]tabContext{},
		refs:               store.New(),
		timeout:            20 * time.Second,
		lastState:          map[string]*SemanticState{},
		observedState:      map[string]*SemanticState{},
		versions:           map[string]int64{},
		trace:              make([]TraceEntry, 0, 16),
		consoleCaptureTabs: map[string]bool{},
		consoleMessages:    map[string][]ConsoleMessage{},
		userDataDir:        t.TempDir(),
		downloadIndex:      map[string]int{},
		downloadVersions:   map[string]uint64{},
		downloadCursors:    map[string]uint64{},
		cancels:            newCancelRegistry(),
		netCaptureTabs:     map[string]bool{},
		shadowPierceTabs:   map[string]bool{},
		webmcpTabs:         map[string]bool{},
		emulationStates:    map[string]deviceEmulationState{},
		incognitoContexts:  map[string]bool{},
	}
	// Mirror production New(): connect() verifies the browser and registers the
	// target-lifecycle listener that reclaims externally-closed tabs.
	if err := m.connect(); err != nil {
		t.Skipf("headless Chrome connect failed: %v", err)
	}
	t.Cleanup(func() {
		// chromedp's context cancel is asynchronous with respect to the Chrome
		// process. Wait for the allocator's process-exit signal before testing's
		// TempDir cleanup runs; otherwise Chrome can still be writing Cache_Data and
		// make RemoveAll fail intermittently under -race / slower toolchains.
		shutdownCtx, shutdownCancel := context.WithTimeout(m.browserCtx, 10*time.Second)
		defer shutdownCancel()
		_ = chromedp.Cancel(shutdownCtx)
		_ = m.Close()
	})
	return m
}

func TestManagerDownloadsCapturesTriggeredDownload(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// Open a blank page and register it as the active tab so the Manager's tab
	// context (and its download listener) is bound to where the download fires.
	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("data:text/html,<html><body>x</body></html>").Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))

	// Arm download tracking before triggering. This sets Browser.setDownloadBehavior
	// and attaches the target-level listener to the active tab context.
	if _, err := m.Downloads(ctx); err != nil {
		t.Fatalf("arm downloads: %v", err)
	}

	// Trigger a download of a small inline blob via standard DOM APIs. No
	// site-specific logic — pure web standards.
	trigger := `(function(){
		var blob = new Blob(["hello-download-fixture"], {type:"text/plain"});
		var a = document.createElement("a");
		a.href = URL.createObjectURL(blob);
		a.download = "hello.txt";
		document.body.appendChild(a);
		a.click();
		return true;
	})()`
	if _, err := m.Evaluate(ctx, trigger); err != nil {
		t.Fatalf("trigger download: %v", err)
	}

	// Wait until the download has both landed its begin event (filename present)
	// and reached a terminal state, then take a public snapshot and assert.
	deadline := time.Now().Add(20 * time.Second)
	var last DownloadEntry
	ready := false
	for time.Now().Before(deadline) {
		m.downloadsMu.Lock()
		for _, d := range m.downloads {
			last = d
			if (d.State == string(downloadStateCompleted) || d.State == string(downloadStateCanceled)) &&
				d.SuggestedFilename != "" {
				ready = true
			}
		}
		m.downloadsMu.Unlock()
		if ready {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if last.GUID == "" {
		t.Fatal("no download was captured within the deadline")
	}
	if !ready {
		t.Fatalf("download never reached a terminal state with a filename; last observed: %+v", last)
	}

	// brw_downloads is non-draining: a listed GUID remains available to a
	// following CaptureArtifact(download_guid) call.
	res, err := m.Downloads(ctx)
	if err != nil {
		t.Fatalf("downloads: %v", err)
	}
	if res.Count == 0 {
		t.Fatal("drained result was empty")
	}
	var found bool
	for _, d := range res.Downloads {
		if d.State == string(downloadStateCompleted) || d.State == string(downloadStateCanceled) {
			assertTerminalDownload(t, d)
			if d.TabID != string(id) {
				t.Fatalf("source tab = %q, want %q", d.TabID, id)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no terminal download in retained snapshot: %+v", res.Downloads)
	}
	// Confirm a second read returns the same lifecycle state.
	res2, _ := m.Downloads(ctx)
	if res2.Count != res.Count || res2.Downloads[0].GUID != res.Downloads[0].GUID {
		t.Fatalf("snapshot changed after read: first=%+v second=%+v", res, res2)
	}
}

func assertTerminalDownload(t *testing.T, d DownloadEntry) {
	t.Helper()
	if d.GUID == "" {
		t.Errorf("download GUID is empty: %+v", d)
	}
	if !strings.Contains(d.SuggestedFilename, "hello") {
		t.Errorf("suggested_filename = %q, want it to contain %q", d.SuggestedFilename, "hello")
	}
	if d.State != string(downloadStateCompleted) && d.State != string(downloadStateCanceled) {
		t.Errorf("state = %q, want a terminal state", d.State)
	}
}

// TestManagerDownloadsSnapshotsRecipeDeltasAndBounds is a fast, browser-free
// unit test of snapshot, recipe-baseline, provenance, and eviction behavior.
func TestManagerDownloadsSnapshotsRecipeDeltasAndBounds(t *testing.T) {
	// Pre-mark download tracking as enabled so Downloads() does not try to wire a
	// real browser; this isolates the record + drain + eviction logic.
	m := &Manager{
		downloadIndex: map[string]int{}, downloadVersions: map[string]uint64{},
		downloadCursors: map[string]uint64{}, downloadDir: t.TempDir(), downloadsEnabled: true,
	}

	m.recordDownloadBeginForTab("tab-1", &cdpbrowser.EventDownloadWillBegin{GUID: "g1", URL: "https://example.com/a.bin", SuggestedFilename: "a.bin"})
	m.recordDownloadProgressForTab("tab-1", &cdpbrowser.EventDownloadProgress{GUID: "g1", ReceivedBytes: 10, TotalBytes: 100, State: downloadStateInProgress})

	res, err := m.Downloads(context.Background())
	if err != nil {
		t.Fatalf("downloads: %v", err)
	}
	if res.Count != 1 || len(res.Downloads) != 1 {
		t.Fatalf("count = %d, want 1", res.Count)
	}
	got := res.Downloads[0]
	if got.GUID != "g1" || got.SuggestedFilename != "a.bin" || got.State != string(downloadStateInProgress) || got.TabID != "tab-1" {
		t.Fatalf("unexpected entry: %+v", got)
	}
	if got.ReceivedBytes != 10 || got.TotalBytes != 100 {
		t.Fatalf("unexpected progress fields: %+v", got)
	}

	// Ordinary snapshots do not consume the entry.
	res2, _ := m.Downloads(context.Background())
	if res2.Count != 1 || res2.Downloads[0].SuggestedFilename != "a.bin" {
		t.Fatalf("second snapshot lost state: %+v", res2)
	}

	// A guarded recipe call establishes a per-tab baseline. An in-progress
	// event returned by that baseline remains in the registry; its later
	// completion is returned as a new delta with begin-event fields intact.
	recipeCtx := WithAllowedOrigins(WithTabID(context.Background(), "tab-1"), []string{"https://example.com"})
	baseline, _ := m.Downloads(recipeCtx)
	if baseline.Count != 1 {
		t.Fatalf("recipe baseline = %+v, want existing entry", baseline)
	}
	unchanged, _ := m.Downloads(recipeCtx)
	if unchanged.Count != 0 {
		t.Fatalf("unchanged recipe delta = %+v, want empty", unchanged)
	}
	m.recordDownloadProgressForTab("tab-1", &cdpbrowser.EventDownloadProgress{GUID: "g1", ReceivedBytes: 100, TotalBytes: 100, State: downloadStateCompleted, FilePath: "/tmp/a.bin"})
	completed, _ := m.Downloads(recipeCtx)
	if completed.Count != 1 || completed.Downloads[0].State != string(downloadStateCompleted) || completed.Downloads[0].SuggestedFilename != "a.bin" {
		t.Fatalf("completion delta lost lifecycle state: %+v", completed)
	}
	otherTabCtx := WithAllowedOrigins(WithTabID(context.Background(), "tab-2"), []string{"https://example.com"})
	other, _ := m.Downloads(otherTabCtx)
	if other.Count != 0 {
		t.Fatalf("different tab observed attributed download: %+v", other)
	}
	m.recordDownloadBegin(&cdpbrowser.EventDownloadWillBegin{GUID: "unknown", URL: "https://example.com/u", SuggestedFilename: "unknown.bin"})
	unknown, _ := m.Downloads(otherTabCtx)
	if unknown.Count != 0 {
		t.Fatalf("recipe observed unattributed download: %+v", unknown)
	}
	manual, _ := m.Downloads(context.Background())
	if manual.Count != 2 {
		t.Fatalf("manual snapshot should retain attributed and unattributed entries: %+v", manual)
	}

	// Eviction: pushing past the cap keeps only the most recent entries.
	for i := 0; i < maxTrackedDownloads+50; i++ {
		m.recordDownloadBegin(&cdpbrowser.EventDownloadWillBegin{GUID: fmt.Sprintf("g%d", i), URL: "https://example.com", SuggestedFilename: "f"})
	}
	m.downloadsMu.Lock()
	n := len(m.downloads)
	m.downloadsMu.Unlock()
	if n != maxTrackedDownloads {
		t.Fatalf("buffer size = %d, want %d", n, maxTrackedDownloads)
	}
}

func TestManagerDownloadStagingIsPrivateOwnedAndConfined(t *testing.T) {
	userDataDir := t.TempDir()
	m := &Manager{userDataDir: userDataDir, downloadIndex: map[string]int{}}
	dir, err := m.resolveDownloadDir()
	if err != nil {
		t.Fatal(err)
	}
	m.downloadDir = dir
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 || !strings.HasPrefix(filepath.Base(dir), "session-") {
		t.Fatalf("staging dir=%q info=%v err=%v", dir, info, err)
	}
	baseInfo, err := os.Stat(filepath.Dir(dir))
	if err != nil || baseInfo.Mode().Perm() != 0o700 {
		t.Fatalf("staging base mode/error = %v %v", baseInfo, err)
	}

	guid := "guid-1"
	managedPath := filepath.Join(dir, guid)
	if err := os.WriteFile(managedPath, []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := m.CleanupManagedDownload(DownloadEntry{GUID: guid, Path: managedPath})
	if err != nil || !removed {
		t.Fatalf("managed cleanup removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(managedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed source survived cleanup: %v", err)
	}

	outside := filepath.Join(t.TempDir(), guid)
	if err := os.WriteFile(outside, []byte("user-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err = m.CleanupManagedDownload(DownloadEntry{GUID: guid, Path: outside})
	if err != nil || removed {
		t.Fatalf("outside cleanup removed=%v err=%v", removed, err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "user-owned" {
		t.Fatalf("outside file changed: data=%q err=%v", data, err)
	}
	linkedGUID := "guid-symlink"
	linkedOutside := filepath.Join(t.TempDir(), "outside-source")
	if err := os.WriteFile(linkedOutside, []byte("do not follow"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedPath := filepath.Join(dir, linkedGUID)
	if err := os.Symlink(linkedOutside, linkedPath); err != nil {
		t.Fatal(err)
	}
	removed, err = m.CleanupManagedDownload(DownloadEntry{GUID: linkedGUID, Path: linkedPath})
	if err != nil || !removed {
		t.Fatalf("managed symlink cleanup removed=%v err=%v", removed, err)
	}
	if data, err := os.ReadFile(linkedOutside); err != nil || string(data) != "do not follow" {
		t.Fatalf("managed cleanup followed symlink: data=%q err=%v", data, err)
	}

	if err := m.cleanupDownloadStaging(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned session directory survived close cleanup: %v", err)
	}
}

func TestManagerDownloadStagingRejectsSymlinkAndGitCheckout(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		userDataDir := t.TempDir()
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(userDataDir, "brw-downloads")); err != nil {
			t.Fatal(err)
		}
		m := &Manager{userDataDir: userDataDir}
		if _, err := m.resolveDownloadDir(); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink staging error = %v", err)
		}
	})
	t.Run("git-checkout", func(t *testing.T) {
		checkout := t.TempDir()
		if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		m := &Manager{userDataDir: filepath.Join(checkout, "profile")}
		if _, err := m.resolveDownloadDir(); err == nil || !strings.Contains(err.Error(), "Git checkout") {
			t.Fatalf("checkout staging error = %v", err)
		}
	})
}
