package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/chromeoptin"
)

// chromeOptInFlags are the flags a Chrome opt-in launch cannot be combined
// with, and why. Every one of them either starts a browser or names a second
// browser to reach, and this lane does neither: it attaches to the Chrome the
// user turned remote debugging on in, and only that one.
//
// The table is the gate, so it is enumerated in a test against the flag set
// rather than spot-checked: a flag added to brwd that launches or attaches and
// is missing here would combine silently, and the last writer of cfg would
// decide which browser the daemon drove.
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
		return "--proxy-server/--ignore-https-errors/--ca-cert"
	case f.Extensions > 0:
		return "--extension"
	case f.ChromeArgs > 0:
		return "--chrome-arg"
	case strings.TrimSpace(f.ChromePath) != "":
		return "--chrome-path"
	case f.Port != 0:
		return "--remote-debugging-port"
	default:
		return ""
	}
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
	// UserDataDir is deliberately left alone. The discovered directory is the
	// browser's own — the profile its user is signed into — and the Manager
	// stages downloads under whatever UserDataDir it is given, which would put
	// brw-created directories inside that profile. brw reads that directory to
	// find the endpoint and writes nothing to it; the endpoint carries the path
	// so the daemon can report which profile it attached to.
	return cfg, endpoint, nil
}
