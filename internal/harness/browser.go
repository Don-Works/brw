package harness

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	cdplaunch "github.com/Don-Works/brw/internal/cdp"
)

// BrowserOptions configures the disposable browser a harness run drives.
type BrowserOptions struct {
	ChromePath string
	Timeout    time.Duration
	// Meter interposes a counting relay between brw and Chrome. A run that does
	// not report transport cost does not need it.
	Meter bool
}

// Browser is a headless Chrome on a throwaway profile, plus the manager driving
// it and, when asked for, the meter in between.
type Browser struct {
	Manager *browser.Manager
	Meter   *Meter
	Version ChromeVersion

	launcher    *cdplaunch.Launcher
	userDataDir string
}

// chromeArgs are the launch flags every harness run uses. They exist to make a
// run repeatable rather than fast.
//
// A fixed window and device scale keep layout identical between machines, and
// the first-run surfaces would otherwise steal a tab and change what a snapshot
// sees. The networking flags matter for a different reason: a harness that
// claims to reach nothing but its own fixture origin has to actually reach
// nothing else, and Chrome's default background traffic — component updates,
// GCM registration, safe-browsing, variations — is neither silent nor free.
func chromeArgs() []string {
	return []string{
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--hide-scrollbars",
		"--window-size=1280,800",
		"--force-device-scale-factor=1",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-search-engine-choice-screen",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-client-side-phishing-detection",
		"--disable-sync",
		"--disable-default-apps",
		"--metrics-recording-only",
		"--no-pings",
	}
}

// LaunchBrowser starts Chrome on a temporary profile and connects a manager to
// it. The caller closes the result.
func LaunchBrowser(ctx context.Context, opts BrowserOptions) (*Browser, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	userDataDir, err := os.MkdirTemp("", "brw-harness-*")
	if err != nil {
		return nil, err
	}
	rig := &Browser{userDataDir: userDataDir}

	launcher, err := cdplaunch.Launch(ctx, cdplaunch.LaunchConfig{
		ChromePath:  opts.ChromePath,
		UserDataDir: userDataDir,
		Args:        chromeArgs(),
		Headless:    true,
	})
	if err != nil {
		_ = os.RemoveAll(userDataDir)
		return nil, fmt.Errorf("launch chrome: %w", err)
	}
	rig.launcher = launcher

	version, err := ReadChromeVersion(launcher.Endpoint())
	if err != nil {
		_ = rig.Close()
		return nil, err
	}
	rig.Version = version

	remote := launcher.Endpoint()
	if opts.Meter {
		meter, err := StartMeter(launcher.Endpoint())
		if err != nil {
			_ = rig.Close()
			return nil, err
		}
		rig.Meter = meter
		remote = meter.BrowserWSURL()
	}

	// RemoteURL rather than letting the manager launch: the browser has to exist
	// before the meter can be put in front of it, and the meter has to exist
	// before the manager connects or the first commands go unmetered.
	manager, err := browser.New(ctx, browser.Config{
		RemoteURL:   remote,
		UserDataDir: userDataDir,
		Timeout:     timeout,
	})
	if err != nil {
		_ = rig.Close()
		return nil, fmt.Errorf("connect to chrome: %w", err)
	}
	rig.Manager = manager
	return rig, nil
}

// Close shuts the manager, the meter and the browser down, in that order, and
// removes the throwaway profile. The browser is closed before the final
// resource reading is taken so its CPU and memory are accounted for.
func (b *Browser) Close() error {
	var first error
	if b.Manager != nil {
		if err := b.Manager.Close(); err != nil {
			first = err
		}
		b.Manager = nil
	}
	if b.Meter != nil {
		if err := b.Meter.Close(); err != nil && first == nil {
			first = err
		}
		b.Meter = nil
	}
	if b.launcher != nil {
		// Ignored deliberately: Close stops Chrome with SIGTERM and returns
		// whatever Wait reports, so a normal shutdown surfaces as "signal:
		// terminated". That is not a run failure and must not be reported as one.
		_ = b.launcher.Close()
		b.launcher = nil
	}
	if b.userDataDir != "" {
		_ = os.RemoveAll(b.userDataDir)
		b.userDataDir = ""
	}
	return first
}
