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
	// The four below shape a launch or a profile rather than an endpoint.
	UserDataDirSet          bool
	ProfileDirectorySet     bool
	UnsafeRealProfile       bool
	UnsafeDefaultProfileCDP bool
}

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

func checkChromeOptInProfile(profile profilepolicy.Profile) error {
	if profile.ChromeOptInAllowed {
		return nil
	}
	return fmt.Errorf("profile %q is not allowed on the Chrome opt-in lane by workspace policy: that lane is full browser-target CDP (HttpOnly cookies, incognito contexts, browser-level permission grants) against the profile you are signed into, so it is granted by its own \"chrome_opt_in_allowed\": true and not by direct_cdp_allowed", profile.Name)
}

func chromeOptInBrowserKind(flagValue string, flagChosen bool, profileKind string) string {
	if !flagChosen && strings.TrimSpace(profileKind) != "" {
		return strings.TrimSpace(profileKind)
	}
	return flagValue
}

func chromeOptInDiscoveryDir(explicit, profileDir, goos, browserKind string) string {
	if dir := strings.TrimSpace(explicit); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(profileDir); dir != "" {
		return dir
	}
	return chromeoptin.DefaultUserDataDir(goos, browserKind)
}

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

func checkChromeOptInFlags(f chromeOptInFlags) error {
	name := f.conflict()
	if name == "" {
		return nil
	}
	return fmt.Errorf("%s cannot be combined with --chrome-opt-in: this lane attaches to the Chrome whose user turned on remote debugging at chrome://inspect/#remote-debugging, so brw neither starts a browser nor chooses which one to reach", name)
}

type chromeOptInRequest struct {
	// UserDataDir is the directory discovery reads.
	UserDataDir string
	// Profile is the policy profile this daemon resolved, and HavePolicy says whether one was resolved at all.
	Profile    profilepolicy.Profile
	HavePolicy bool
}

func configureChromeOptIn(ctx context.Context, cfg browser.Config, req chromeOptInRequest) (browser.Config, chromeoptin.Endpoint, error) {
	cfg.AttachOnly = true
	cfg.SignedInProfile = true

	if req.HavePolicy {
		if err := checkChromeOptInProfile(req.Profile); err != nil {
			return cfg, chromeoptin.Endpoint{}, err
		}
	}
	dir := strings.TrimSpace(req.UserDataDir)
	if dir == "" {
		return cfg, chromeoptin.Endpoint{}, fmt.Errorf("--chrome-opt-in needs the Chrome user data directory to look in: brw does not know where this browser keeps profiles on this platform, so pass --chrome-opt-in-user-data-dir")
	}
	endpoint, err := chromeoptin.Discover(ctx, chromeoptin.Options{UserDataDir: dir})
	if err != nil {
		return cfg, chromeoptin.Endpoint{}, err
	}
	if req.HavePolicy {
		if err := checkChromeOptInEndpointProfile(endpoint, req.Profile); err != nil {
			return cfg, chromeoptin.Endpoint{}, err
		}
	}
	cfg.RemoteURL = endpoint.HTTPURL

	cfg.BrowserWSURL = endpoint.BrowserWSURL

	return cfg, endpoint, nil
}
