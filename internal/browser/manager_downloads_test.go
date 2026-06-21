package browser

import (
	"context"
	"fmt"
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
// Downloads drain) without depending on the production visible-Chrome launcher,
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
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		t.Skipf("headless Chrome did not start: %v", err)
	}
	m := &Manager{
		allocCancel:   allocCancel,
		browserCtx:    browserCtx,
		browserCancel: browserCancel,
		tabContexts:   map[string]tabContext{},
		refs:          store.New(),
		timeout:       20 * time.Second,
		lastState:     map[string]*SemanticState{},
		versions:      map[string]int64{},
		trace:         make([]TraceEntry, 0, 16),
		userDataDir:   t.TempDir(),
		downloadIndex: map[string]int{},
	}
	t.Cleanup(func() { m.Close() })
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

	// Poll the buffer WITHOUT draining (Downloads() is drain-on-read, so polling
	// it mid-flight would fragment a single download's lifecycle across reads).
	// Wait until the download has both landed its begin event (filename present)
	// and reached a terminal state, then drain once and assert.
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

	// brw_downloads drains: the buffer is reported once and then cleared.
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
			found = true
		}
	}
	if !found {
		t.Fatalf("no terminal download in drained result: %+v", res.Downloads)
	}
	// Confirm the drain cleared the buffer.
	res2, _ := m.Downloads(ctx)
	if res2.Count != 0 {
		t.Fatalf("buffer not drained after read: %d", res2.Count)
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

// TestManagerDownloadsDrainAndBounds is a fast, browser-free unit test of the
// record + drain + eviction logic behind brw_downloads.
func TestManagerDownloadsDrainAndBounds(t *testing.T) {
	// Pre-mark download tracking as enabled so Downloads() does not try to wire a
	// real browser; this isolates the record + drain + eviction logic.
	m := &Manager{downloadIndex: map[string]int{}, downloadDir: t.TempDir(), downloadsEnabled: true}

	m.recordDownloadBegin(&cdpbrowser.EventDownloadWillBegin{GUID: "g1", URL: "https://example.com/a.bin", SuggestedFilename: "a.bin"})
	m.recordDownloadProgress(&cdpbrowser.EventDownloadProgress{GUID: "g1", ReceivedBytes: 10, TotalBytes: 100, State: downloadStateInProgress})
	m.recordDownloadProgress(&cdpbrowser.EventDownloadProgress{GUID: "g1", ReceivedBytes: 100, TotalBytes: 100, State: downloadStateCompleted, FilePath: "/tmp/a.bin"})

	res, err := m.Downloads(context.Background())
	if err != nil {
		t.Fatalf("downloads: %v", err)
	}
	if res.Count != 1 || len(res.Downloads) != 1 {
		t.Fatalf("count = %d, want 1", res.Count)
	}
	got := res.Downloads[0]
	if got.GUID != "g1" || got.SuggestedFilename != "a.bin" || got.State != string(downloadStateCompleted) {
		t.Fatalf("unexpected entry: %+v", got)
	}
	if got.ReceivedBytes != 100 || got.TotalBytes != 100 || got.Path != "/tmp/a.bin" {
		t.Fatalf("unexpected progress fields: %+v", got)
	}

	// Draining clears the buffer.
	res2, _ := m.Downloads(context.Background())
	if res2.Count != 0 {
		t.Fatalf("buffer not drained: %d", res2.Count)
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
