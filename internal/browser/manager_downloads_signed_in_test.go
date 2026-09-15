package browser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/store"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Browser.setDownloadBehavior has no scope narrower than a browser context, so
// on a lane whose browser context belongs to the person using it, a download
// path is not brw's to set. These pin the refusal at the only two entry points
// that carry one, and they need no browser: the check precedes every CDP call
// precisely so that no path can be sent before it is reached.
func TestSignedInLaneRefusesToRouteDownloads(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(m *Manager) error
	}{
		{
			name: "the command that carries a path",
			call: func(m *Manager) error {
				return m.applyDownloadBehavior(context.Background(), t.TempDir())
			},
		},
		{
			name: "brw_set_download_path naming a directory",
			call: func(m *Manager) error {
				_, err := m.SetDownloadPath(context.Background(), DownloadPathOptions{Path: t.TempDir()})
				return err
			},
		},
		{
			// clear:true is the one that reads as harmless and is not: it adopts a
			// brw-owned staging directory for the whole browser context.
			name: "brw_set_download_path clearing back to brw's staging",
			call: func(m *Manager) error {
				_, err := m.SetDownloadPath(context.Background(), DownloadPathOptions{Clear: true})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{signedInProfile: true}
			if err := tc.call(m); !errors.Is(err, ErrDownloadRoutingSignedIn) {
				t.Fatalf("got %v, want ErrDownloadRoutingSignedIn", err)
			}
		})
	}
}

// The lane still has to report downloads, and report them honestly: no staging
// directory, no file path, and the note that says why.
//
// The browser here is launched rather than attached, but it carries the two
// properties that matter — SignedInProfile and a download directory its user
// configured — so the assertion is about the lane's behaviour and not about how
// the endpoint was reached.
func TestSignedInLaneLeavesTheBrowsersOwnDownloadsAlone(t *testing.T) {
	humanDir := t.TempDir()
	m := newSignedInHeadlessManager(t, humanDir)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("data:text/html,<html><body>x</body></html>").Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))

	result, err := m.Downloads(ctx)
	if err != nil {
		t.Fatalf("arm downloads: %v", err)
	}
	if !result.Supported {
		t.Fatal("downloads reported unsupported; this lane observes them")
	}
	if result.FilePaths {
		t.Fatal("downloads reported file paths; brw stages nothing on a browser its user is signed into")
	}
	if result.Note == "" {
		t.Fatal("no note said why entries carry no path")
	}

	m.downloadsMu.Lock()
	stagingDir, owned := m.downloadDir, m.downloadDirOwned
	m.downloadsMu.Unlock()
	if stagingDir != "" || owned {
		t.Fatalf("staging directory %q owned=%v; arming tracking must create none, because Manager.Close deletes what it owns", stagingDir, owned)
	}

	trigger := `(function(){
		var blob = new Blob(["signed-in-download-fixture"], {type:"text/plain"});
		var a = document.createElement("a");
		a.href = URL.createObjectURL(blob);
		a.download = "human.txt";
		document.body.appendChild(a);
		a.click();
		return true;
	})()`
	if _, err := m.Evaluate(WithTabID(ctx, string(id)), trigger); err != nil {
		t.Fatalf("trigger download: %v", err)
	}

	// The file must land where the person's browser was already sending it,
	// under the name the page asked for. Retargeting puts it in brw's staging
	// directory named by download GUID instead, and Manager.Close then removes
	// that directory.
	landed := filepath.Join(humanDir, "human.txt")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(landed); err == nil && string(data) == "signed-in-download-fixture" {
			assertObservedWithoutAPath(ctx, t, m)
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	entries, _ := os.ReadDir(humanDir)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	t.Fatalf("the browser's own download directory holds %v, not human.txt: brw redirected a download it does not own", names)
}

// assertObservedWithoutAPath is the other half of the bargain: brw gives up
// choosing where the file goes, so it owes the caller the record of it going.
// Leaving the behaviour at Chrome's default while turning the event stream on
// is what buys that, and this is the assertion that says so.
func assertObservedWithoutAPath(ctx context.Context, t *testing.T, m *Manager) {
	t.Helper()
	snapshot, err := m.Downloads(ctx)
	if err != nil {
		t.Fatalf("downloads after the file landed: %v", err)
	}
	for _, entry := range snapshot.Downloads {
		if entry.SuggestedFilename != "human.txt" {
			continue
		}
		if entry.State != string(downloadStateCompleted) {
			t.Fatalf("download observed as %q, want completed", entry.State)
		}
		if entry.Path != "" {
			t.Fatalf("download reported path %q; brw did not stage this file and must not name one", entry.Path)
		}
		return
	}
	t.Fatalf("the download never appeared in brw_downloads: %+v", snapshot.Downloads)
}

// newSignedInHeadlessManager launches a headless Chrome whose user has
// configured a download directory, and drives it as the signed-in lane does.
//
// The directory is set through the profile's Preferences file rather than over
// CDP on purpose: that is where a real person's choice lives, and a CDP
// override would be undone by the very command under test.
func newSignedInHeadlessManager(t *testing.T, downloadDir string) *Manager {
	t.Helper()
	chromePath, err := cdp.FindChrome("")
	if err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	userDataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(userDataDir, "Default"), 0o700); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	prefs, err := json.Marshal(map[string]any{
		"download": map[string]any{
			"default_directory":   downloadDir,
			"directory_upgrade":   true,
			"prompt_for_download": false,
		},
		"profile": map[string]any{"exit_type": "Normal", "exited_cleanly": true},
	})
	if err != nil {
		t.Fatalf("encode preferences: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDataDir, "Default", "Preferences"), prefs, 0o600); err != nil {
		t.Fatalf("write preferences: %v", err)
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("disable-gpu", true),
		chromedp.UserDataDir(userDataDir),
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
		userDataDir:        userDataDir,
		downloadIndex:      map[string]int{},
		downloadVersions:   map[string]uint64{},
		downloadCursors:    map[string]uint64{},
		cancels:            newCancelRegistry(),
		netCaptureTabs:     map[string]bool{},
		shadowPierceTabs:   map[string]bool{},
		webmcpTabs:         map[string]bool{},
		emulationStates:    map[string]deviceEmulationState{},
		incognitoContexts:  map[string]bool{},
		signedInProfile:    true,
		attachOnly:         true,
	}
	if err := m.connect(); err != nil {
		t.Skipf("headless Chrome connect failed: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(m.browserCtx, 10*time.Second)
		defer shutdownCancel()
		_ = chromedp.Cancel(shutdownCtx)
		_ = m.Close()
	})
	return m
}
