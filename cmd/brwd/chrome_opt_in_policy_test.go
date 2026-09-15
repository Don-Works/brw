package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// The lane is full browser-target CDP against the profile its user is signed
// into, so a policy has to grant it explicitly. Reading direct_cdp_allowed here
// would be the wrong question — that bit is about brw launching a browser
// against a profile directory — and a bridge-only profile is exactly the one
// the restriction exists to protect.
//
// The call under test is configureChromeOptIn, which is the whole of the lane's
// setup, and not checkChromeOptInProfile. An earlier spelling put the gate in
// main() and tested the checker: the four cases below passed with the call
// deleted from main(), because nothing they ran went near it. The counter is
// the other half — a refusal that had already probed the browser would have let
// a policy-restricted profile be reached before being refused.
func TestChromeOptInNeedsItsOwnPolicyGrant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile profilepolicy.Profile
		allowed bool
	}{
		{
			name:    "bridge-only profile",
			profile: profilepolicy.Profile{Name: "work", ExtensionBridgeAllowed: true},
		},
		{
			name:    "direct-cdp profile does not imply the opt-in lane",
			profile: profilepolicy.Profile{Name: "throwaway", DirectCDPAllowed: true},
		},
		{
			name:    "a profile that allows everything but this lane",
			profile: profilepolicy.Profile{Name: "both", DirectCDPAllowed: true, ExtensionBridgeAllowed: true},
		},
		{
			name:    "granted",
			profile: profilepolicy.Profile{Name: "signed-in", ExtensionBridgeAllowed: true, ChromeOptInAllowed: true},
			allowed: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, hits := fakeOptInChrome(t)
			cfg, endpoint, err := configureChromeOptIn(context.Background(), browser.Config{}, chromeOptInRequest{
				UserDataDir: dir,
				Profile:     tc.profile,
				HavePolicy:  true,
			})
			if tc.allowed {
				if err != nil {
					t.Fatalf("profile with chrome_opt_in_allowed was refused: %v", err)
				}
				if cfg.RemoteURL == "" || cfg.RemoteURL != endpoint.HTTPURL {
					t.Fatalf("granted profile got RemoteURL %q, endpoint %q", cfg.RemoteURL, endpoint.HTTPURL)
				}
				return
			}
			if err == nil {
				t.Fatal("the opt-in lane was allowed for a profile whose policy never granted it")
			}
			if !strings.Contains(err.Error(), tc.profile.Name) {
				t.Fatalf("refusal %q does not name the profile", err)
			}
			if cfg.RemoteURL != "" {
				t.Fatalf("the refusal still handed back RemoteURL %q", cfg.RemoteURL)
			}
			if got := hits.Load(); got != 0 {
				t.Fatalf("a profile the policy never granted had its browser probed %d time(s); the gate has to run before discovery", got)
			}
		})
	}
}

// brw_identity sells itself as reporting which profile this namespace drives.
// Nothing else checks that the Chrome answering on the discovered port is the
// profile the policy named.
//
// Driven through configureChromeOptIn for the same reason as the grant above:
// this check ran in main() and every case passed without it.
func TestChromeOptInEndpointMustBeThePolicysProfile(t *testing.T) {
	granted := func(userDataDir string) profilepolicy.Profile {
		return profilepolicy.Profile{Name: "work", ChromeOptInAllowed: true, UserDataDir: userDataDir}
	}
	for _, tc := range []struct {
		name string
		// policy maps the directory discovery will run in to the one the
		// profile names, so a case can name the same place differently.
		policy  func(discovered string) string
		refused bool
	}{
		{name: "same directory", policy: func(d string) string { return d }},
		{name: "same directory spelled differently", policy: func(d string) string { return filepath.Join(d, "sub", "..") }},
		{name: "another profile entirely", policy: func(d string) string { return filepath.Join(d, "another-browser") }, refused: true},
		{name: "policy names no directory", policy: func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := fakeOptInChrome(t)
			cfg, _, err := configureChromeOptIn(context.Background(), browser.Config{}, chromeOptInRequest{
				UserDataDir: dir,
				Profile:     granted(tc.policy(dir)),
				HavePolicy:  true,
			})
			if tc.refused {
				if err == nil {
					t.Fatal("a daemon driving one profile while reporting another was accepted")
				}
				if cfg.RemoteURL != "" {
					t.Fatalf("the refusal still handed back RemoteURL %q, so the daemon would attach anyway", cfg.RemoteURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused a matching endpoint: %v", err)
			}
			if cfg.RemoteURL == "" {
				t.Fatal("a matching endpoint produced no RemoteURL")
			}
		})
	}
}

// A daemon with no --profile and no --workspace has no policy to consult. That
// is the shipped single-profile case, and it stays working: the gate applies to
// a policy that exists and does not grant, not to the absence of one.
func TestChromeOptInWithoutAPolicyIsNotGated(t *testing.T) {
	dir, _ := fakeOptInChrome(t)
	cfg, _, err := configureChromeOptIn(context.Background(), browser.Config{}, chromeOptInRequest{UserDataDir: dir})
	if err != nil {
		t.Fatalf("--chrome-opt-in with no profile policy was refused: %v", err)
	}
	if cfg.RemoteURL == "" {
		t.Fatal("no endpoint was configured")
	}
}

// Which browser's directory to look in is a property of the profile, not a
// constant. Defaulting to Chrome against a Chromium-bound profile reports one
// browser and drives another.
func TestChromeOptInBrowserFollowsTheProfile(t *testing.T) {
	for _, tc := range []struct {
		name        string
		flagValue   string
		flagChosen  bool
		profileKind string
		want        string
	}{
		{name: "profile decides when the flag was left alone", flagValue: "chrome", profileKind: "chromium", want: "chromium"},
		{name: "an explicit flag still wins", flagValue: "brave", flagChosen: true, profileKind: "chromium", want: "brave"},
		{name: "no profile leaves the default", flagValue: "chrome", want: "chrome"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chromeOptInBrowserKind(tc.flagValue, tc.flagChosen, tc.profileKind); got != tc.want {
				t.Fatalf("browser = %q, want %q", got, tc.want)
			}
		})
	}
}

// Discovery looks where the operator said, then where the profile says, and
// only then at the platform default.
func TestChromeOptInDiscoveryDirPrefersWhatIsKnown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		explicit   string
		profileDir string
		want       string
	}{
		{name: "explicit wins", explicit: "/tmp/explicit", profileDir: "/tmp/profile", want: "/tmp/explicit"},
		{name: "profile next", profileDir: "/tmp/profile", want: "/tmp/profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := chromeOptInDiscoveryDir(tc.explicit, tc.profileDir, "linux", "chrome")
			if got != tc.want {
				t.Fatalf("discovery dir = %q, want %q", got, tc.want)
			}
		})
	}
	// With neither, it falls through to the platform table rather than to "".
	if got := chromeOptInDiscoveryDir("", "", "linux", "chrome"); got == "" {
		t.Fatal("no directory at all and no platform default; discovery would be told nothing")
	}
}
