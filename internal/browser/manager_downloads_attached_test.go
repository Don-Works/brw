package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/brwidentity"
	cdplaunch "github.com/Don-Works/brw/internal/cdp"
)

// Browser.setDownloadBehavior has no scope narrower than a browser context, so
// in a browser brw did not start a download path is not brw's to set. These pin
// the refusal at the only two entry points that carry one, and they need no
// browser: the check precedes every CDP call precisely so that no path can be
// sent before it is reached.
func TestAttachedBrowserRefusesToRouteDownloads(t *testing.T) {
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
		{
			// The function that creates the directory refuses too, so a caller
			// added later cannot make one inside somebody else's profile by
			// forgetting to ask first.
			name: "resolving a staging directory at all",
			call: func(m *Manager) error {
				_, err := m.resolveDownloadDir()
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{attachedBrowser: true}
			if err := tc.call(m); !errors.Is(err, ErrDownloadRoutingAttachedBrowser) {
				t.Fatalf("got %v, want ErrDownloadRoutingAttachedBrowser", err)
			}
		})
	}
}

// The second route to the data loss, driven end to end on the lane it was
// reached through: `brwd --remote`.
//
// The first fix keyed the refusal to Config.SignedInProfile, which exactly one
// caller sets — the Chrome opt-in lane — so --remote pointed at the same
// browser, up to and including the port the opt-in publishes, still retargeted
// every download in it and then deleted brw's staging directory at Close. The
// lane carries no flag saying whose browser it is, and there is none to add:
// what it does carry is that brw did not start the browser.
//
// So the browser here is started outside brw, with a download directory its
// user configured, and reached exactly as --remote reaches one: New with
// RemoteURL and nothing else. No SignedInProfile, no AttachOnly, no lane name.
func TestAttachedBrowserLeavesItsOwnDownloadsAlone(t *testing.T) {
	humanDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, port := startChromeOutsideBrw(ctx, t, humanDir)
	m, err := New(ctx, Config{
		RemoteURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		Timeout:   20 * time.Second,
	})
	if err != nil {
		t.Skipf("could not attach to the browser the test started: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if _, err := m.Open(ctx, "data:text/html,<html><body>x</body></html>"); err != nil {
		t.Fatalf("open a tab over the remote endpoint: %v", err)
	}

	result, err := m.Downloads(ctx)
	if err != nil {
		t.Fatalf("arm downloads: %v", err)
	}
	if !result.Supported {
		t.Fatal("downloads reported unsupported; this lane observes them")
	}
	if result.FilePaths {
		t.Fatal("downloads reported file paths; brw stages nothing in a browser it did not start")
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

	if _, err := m.Evaluate(ctx, downloadTriggerJS("human.txt", "attached-download-fixture")); err != nil {
		t.Fatalf("trigger download: %v", err)
	}

	// The file must land where the person's browser was already sending it,
	// under the name the page asked for. Retargeting puts it in brw's staging
	// directory named by download GUID instead, and Manager.Close then removes
	// that directory.
	landed := filepath.Join(humanDir, "human.txt")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(landed); err == nil && string(data) == "attached-download-fixture" {
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

// The control, on the same code path: a browser brw DID start stages downloads
// and reports their paths. Without it the refusal above would also be satisfied
// by a brw that never stages anywhere.
func TestBrwStartedBrowserStillStagesDownloads(t *testing.T) {
	if _, err := cdplaunch.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	profile := browsertest.NewProfile(t)
	m, err := New(ctx, Config{
		UserDataDir: profile.Dir(),
		Timeout:     20 * time.Second,
		ChromeArgs:  []string{"--headless=new", "--disable-gpu", "--no-sandbox"},
	})
	if err != nil {
		t.Skipf("could not launch headless Chrome: %v", err)
	}
	profile.StopWith(func() { _ = m.Close() })

	if _, err := m.Open(ctx, "data:text/html,<html><body>x</body></html>"); err != nil {
		t.Fatalf("open a tab: %v", err)
	}
	result, err := m.Downloads(ctx)
	if err != nil {
		t.Fatalf("arm downloads: %v", err)
	}
	if !result.FilePaths {
		t.Fatalf("a browser brw started reported file_paths=false and note %q; the refusal is meant to apply to browsers brw attached to", result.Note)
	}
	m.downloadsMu.Lock()
	stagingDir := m.downloadDir
	m.downloadsMu.Unlock()
	if stagingDir == "" {
		t.Fatal("no staging directory was created for a browser brw started")
	}

	if _, err := m.Evaluate(ctx, downloadTriggerJS("staged.txt", "staged-download-fixture")); err != nil {
		t.Fatalf("trigger download: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := m.Downloads(ctx)
		if err != nil {
			t.Fatalf("downloads: %v", err)
		}
		for _, entry := range snapshot.Downloads {
			if entry.State != string(downloadStateCompleted) || entry.Path == "" {
				continue
			}
			if !strings.HasPrefix(entry.Path, stagingDir) {
				t.Fatalf("download staged at %q, outside %q", entry.Path, stagingDir)
			}
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("no download completed with a staged path within the deadline")
}

// Every lane brw can report has to say whether brw started the browser on it,
// and the transport table's RuntimeDownloadRouting has to be that same answer.
// A transport added to brwidentity and not classified here fails rather than
// inheriting somebody else's row — which is how `--remote` came to be reported
// as direct CDP and advertised brw_set_download_path it always refuses.
func TestEveryTransportDeclaresWhetherBrwStartedTheBrowser(t *testing.T) {
	brwStarts := map[string]bool{
		brwidentity.TransportDirectCDP:       true,
		brwidentity.TransportRemoteCDP:       false,
		brwidentity.TransportChromeOptIn:     false,
		brwidentity.TransportExtensionBridge: false,
	}
	for _, transport := range brwidentity.Transports() {
		started, classified := brwStarts[transport]
		if !classified {
			t.Errorf("transport %q is not classified here: say whether brw starts the browser on that lane, because that is what decides whether brw may move its downloads", transport)
			continue
		}
		caps, known := brwidentity.Capabilities(transport)
		if !known {
			t.Errorf("transport %q has no capabilities", transport)
			continue
		}
		if caps.RuntimeDownloadRouting != started {
			t.Errorf("transport %q declares RuntimeDownloadRouting=%v, but brw starting the browser there is %v", transport, caps.RuntimeDownloadRouting, started)
		}
		// And the runtime answer, which is what actually refuses, has to agree
		// with what the lane advertises.
		m := &Manager{attachedBrowser: !started}
		if m.stagesDownloads() != caps.RuntimeDownloadRouting {
			t.Errorf("transport %q advertises RuntimeDownloadRouting=%v but a Manager on that lane stages=%v", transport, caps.RuntimeDownloadRouting, m.stagesDownloads())
		}
	}
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

// downloadTriggerJS downloads a blob through standard DOM APIs, which is a
// download Chrome routes exactly as it routes any other.
func downloadTriggerJS(filename, body string) string {
	return `(function(){
		var blob = new Blob([` + jsString(body) + `], {type:"text/plain"});
		var a = document.createElement("a");
		a.href = URL.createObjectURL(blob);
		a.download = ` + jsString(filename) + `;
		document.body.appendChild(a);
		a.click();
		return true;
	})()`
}

// startChromeOutsideBrw starts a Chrome brw did not launch and returns its user
// data directory and the port it recorded. It stands in for every lane where
// somebody else started the browser: --remote, and the Chrome opt-in lane once
// a person has flipped the switch.
//
// It is deliberately not brw's launcher: on those lanes brw never starts a
// browser, so a fixture that used the launcher would exercise a path the lane
// forbids.
//
// The port is 0 because that is what makes Chrome record it. Measured on Chrome
// 153: with an explicit --remote-debugging-port Chrome writes no
// DevToolsActivePort file at all — there is nothing to discover when the caller
// already chose the port — and with 0 it writes the bound port and the browser
// target's path.
//
// downloadDir, when set, is written into the profile's Preferences rather than
// applied over CDP: that is where a real person's choice lives, and a CDP
// override would be undone by the very command under test.
func startChromeOutsideBrw(ctx context.Context, t *testing.T, downloadDir string) (userDataDir string, port int) {
	t.Helper()
	chromePath, err := cdplaunch.FindChrome("")
	if err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	userDataDir = t.TempDir()
	if downloadDir != "" {
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
	}
	cmd := exec.CommandContext(ctx, chromePath,
		"--headless=new",
		"--disable-gpu",
		"--user-data-dir="+userDataDir,
		"--remote-debugging-port=0",
		"--no-first-run",
		"--no-default-browser-check",
		"about:blank",
	)
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start Chrome: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	})

	path := filepath.Join(userDataDir, "DevToolsActivePort")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			first, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
			if port, err = strconv.Atoi(strings.TrimSpace(first)); err == nil && port > 0 {
				return userDataDir, port
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Skipf("Chrome never recorded a debugging port in %s", path)
	return "", 0
}
