package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/chromeoptin"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// chromeOptInFlags are the flags a Chrome opt-in launch cannot be combined
// with, and why. Every one of them either starts a browser, names a second
// browser to reach, or decides which profile a launch would use, and this lane
// does none of those: it attaches to the Chrome the user turned remote
// debugging on in, and only that one.
//
// The table is the gate, and it is hand-maintained. What keeps it honest is
// TestEveryBrwdFlagIsClassifiedAgainstTheOptInLane, which walks brwd's own
// flag registrations and requires each name to appear either here or in that
// test's list of flags that cannot shape which browser is driven. A new flag
// therefore fails the build's tests until somebody says which it is.
type chromeOptInFlags struct {
	Bridge           bool
	RemoteURL        string
	UpstreamHTTP     string
	Headless         bool
	Login            bool
	LaunchNetworking bool
	Extensions       int
	ChromeArgs       int
	ChromePath       string
	Port             int
	// The four below shape a launch or a profile rather than an endpoint. They
	// were accepted and silently ignored: --user-data-dir and
	// --profile-directory because this lane never launches, and the two unsafe
	// overrides because they exist to relax rules about launching. A flag that
	// cannot apply is refused rather than ignored, so the operator is not left
	// believing it did something.
	UserDataDirSet          bool
	ProfileDirectorySet     bool
	UnsafeRealProfile       bool
	UnsafeDefaultProfileCDP bool
}

// conflict names the first incompatible flag, or "" when the combination is
// allowed. Returning the name rather than a bool is what lets the daemon say
// which flag to drop instead of "invalid combination".
func (f chromeOptInFlags) conflict() string {
	switch {
	case f.Bridge:
		return "--bridge"
	case strings.TrimSpace(f.RemoteURL) != "":
		return "--remote"
	case strings.TrimSpace(f.UpstreamHTTP) != "":
		return "--upstream-http"
	case f.Headless:
		return "--headless"
	case f.Login:
		return "--login"
	case f.LaunchNetworking:
		return "--proxy-server/--proxy-bypass-list/--ignore-https-errors/--ca-cert"
	case f.Extensions > 0:
		return "--extension"
	case f.ChromeArgs > 0:
		return "--chrome-arg"
	case strings.TrimSpace(f.ChromePath) != "":
		return "--chrome-path"
	case f.Port != 0:
		return "--remote-debugging-port"
	case f.UserDataDirSet:
		return "--user-data-dir"
	case f.ProfileDirectorySet:
		return "--profile-directory"
	case f.UnsafeRealProfile:
		return "--unsafe-real-profile"
	case f.UnsafeDefaultProfileCDP:
		return "--unsafe-allow-default-profile-cdp"
	default:
		return ""
	}
}

// checkChromeOptInProfile refuses the lane for a profile whose policy has not
// named it. It is a separate bit from direct_cdp_allowed on purpose: that one
// says brw may launch its own browser against a profile directory, and this one
// says brw may drive the browser the person is signed into.
func checkChromeOptInProfile(profile profilepolicy.Profile) error {
	if profile.ChromeOptInAllowed {
		return nil
	}
	return fmt.Errorf("profile %q is not allowed on the Chrome opt-in lane by workspace policy: that lane is full browser-target CDP (HttpOnly cookies, incognito contexts, browser-level permission grants) against the profile you are signed into, so it is granted by its own \"chrome_opt_in_allowed\": true and not by direct_cdp_allowed", profile.Name)
}

// chromeOptInBrowserKind decides whose user data directory to look in. A policy
// profile names the browser it is about, and defaulting to "chrome" against a
// Chromium-bound profile would have the daemon report one browser and drive
// another.
func chromeOptInBrowserKind(flagValue string, flagChosen bool, profileKind string) string {
	if !flagChosen && strings.TrimSpace(profileKind) != "" {
		return strings.TrimSpace(profileKind)
	}
	return flagValue
}

// chromeOptInDiscoveryDir picks the directory discovery reads, preferring what
// the operator said, then the profile the policy resolved, then the platform
// default for that browser.
func chromeOptInDiscoveryDir(explicit, profileDir, goos, browserKind string) string {
	if dir := strings.TrimSpace(explicit); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(profileDir); dir != "" {
		return dir
	}
	return chromeoptin.DefaultUserDataDir(goos, browserKind)
}

// checkChromeOptInEndpointProfile refuses an endpoint that is not the profile
// the policy resolved. brw_identity reports the policy's profile name and
// directory, and nothing else checks that the Chrome answering on the
// discovered port is that profile's; without this, `--chrome-opt-in --profile p
// --chrome-opt-in-user-data-dir <other>` reports p and drives <other>.
func checkChromeOptInEndpointProfile(endpoint chromeoptin.Endpoint, profile profilepolicy.Profile) error {
	want := strings.TrimSpace(profile.UserDataDir)
	if want == "" {
		return nil
	}
	if filepath.Clean(endpoint.UserDataDir) == filepath.Clean(want) {
		return nil
	}
	return fmt.Errorf("the opt-in endpoint was discovered in %s, but profile %q names %s: brw_identity would report a profile the daemon is not driving", endpoint.UserDataDir, profile.Name, want)
}

// checkChromeOptInFlags rejects an incompatible combination with a message that
// says why the flag cannot apply here, not merely that it cannot.
func checkChromeOptInFlags(f chromeOptInFlags) error {
	name := f.conflict()
	if name == "" {
		return nil
	}
	return fmt.Errorf("%s cannot be combined with --chrome-opt-in: this lane attaches to the Chrome whose user turned on remote debugging at chrome://inspect/#remote-debugging, so brw neither starts a browser nor chooses which one to reach", name)
}

// configureChromeOptIn resolves the opt-in endpoint and returns the browser
// config for it.
//
// AttachOnly is set unconditionally. Without it, a discovery failure that some
// later edit swallowed would leave RemoteURL empty and browser.New would launch
// Chrome with a debugging flag — brw arranging for itself the access Chrome
// asks a human to grant. The flag makes that a refusal in browser.New rather
// than a rule this function has to remember.
func configureChromeOptIn(ctx context.Context, cfg browser.Config, userDataDir string) (browser.Config, chromeoptin.Endpoint, error) {
	cfg.AttachOnly = true
	cfg.SignedInProfile = true
	dir := strings.TrimSpace(userDataDir)
	if dir == "" {
		return cfg, chromeoptin.Endpoint{}, fmt.Errorf("--chrome-opt-in needs the Chrome user data directory to look in: brw does not know where this browser keeps profiles on this platform, so pass --chrome-opt-in-user-data-dir")
	}
	endpoint, err := chromeoptin.Discover(ctx, chromeoptin.Options{UserDataDir: dir})
	if err != nil {
		return cfg, chromeoptin.Endpoint{}, err
	}
	cfg.RemoteURL = endpoint.HTTPURL
	// The browser WebSocket URL discovery checked is the one brw dials. Without
	// this, chromedp re-asks the endpoint for it at connect time and dials the
	// answer unchecked, so validateBrowserWS would be guarding a URL that never
	// reaches the socket.
	cfg.BrowserWSURL = endpoint.BrowserWSURL
	// UserDataDir is deliberately left alone. The discovered directory is the
	// browser's own — the profile its user is signed into — and the Manager
	// treats UserDataDir as a directory it may create staging under. brw reads
	// that directory to find the endpoint and writes nothing to it; the endpoint
	// carries the path so the daemon can report which profile it attached to.
	return cfg, endpoint, nil
}
