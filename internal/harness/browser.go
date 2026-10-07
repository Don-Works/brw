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
	// Meter interposes a counting relay between brw and Chrome.
	Meter bool
}

// Browser is a headless Chrome on a throwaway profile, plus the manager driving it and, when asked for, the meter in between.
type Browser struct {
	Manager *browser.Manager
	Meter   *Meter
	Version ChromeVersion

	launcher    *cdplaunch.Launcher
	userDataDir string
}

const offBoxBlockedRule = "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"

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
		offBoxBlockedRule,
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-client-side-phishing-detection",
		"--disable-sync",
		"--disable-default-apps",
		"--metrics-recording-only",
		"--no-pings",
	}
}

// LaunchBrowser starts Chrome on a temporary profile and connects a manager to it.
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

// Close shuts the manager, the meter and the browser down, in that order, and removes the throwaway profile.
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

		_ = b.launcher.Close()
		b.launcher = nil
	}
	if b.userDataDir != "" {
		_ = os.RemoveAll(b.userDataDir)
		b.userDataDir = ""
	}
	return first
}
