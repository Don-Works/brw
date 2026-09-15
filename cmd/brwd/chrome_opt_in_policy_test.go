package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/chromeoptin"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// The lane is full browser-target CDP against the profile its user is signed
// into, so a policy has to grant it explicitly. Reading direct_cdp_allowed here
// would be the wrong question — that bit is about brw launching a browser
// against a profile directory — and a bridge-only profile is exactly the one
// the restriction exists to protect.
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
			err := checkChromeOptInProfile(tc.profile)
			if tc.allowed {
				if err != nil {
					t.Fatalf("profile with chrome_opt_in_allowed was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the opt-in lane was allowed for a profile whose policy never granted it")
			}
			if !strings.Contains(err.Error(), tc.profile.Name) {
				t.Fatalf("refusal %q does not name the profile", err)
			}
		})
	}
}

// brw_identity sells itself as reporting which profile this namespace drives.
// Nothing else checks that the Chrome answering on the discovered port is the
// profile the policy named.
func TestChromeOptInEndpointMustBeThePolicysProfile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		policy   string
		refused  bool
	}{
		{name: "same directory", endpoint: "/tmp/brw-fixture/chrome", policy: "/tmp/brw-fixture/chrome"},
		{name: "same directory spelled differently", endpoint: "/tmp/brw-fixture/chrome", policy: "/tmp/brw-fixture/./chrome"},
		{name: "another profile entirely", endpoint: "/tmp/brw-fixture/chromium", policy: "/tmp/brw-fixture/chrome", refused: true},
		{name: "policy names no directory", endpoint: "/tmp/brw-fixture/chrome", policy: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkChromeOptInEndpointProfile(
				chromeoptin.Endpoint{UserDataDir: filepath.FromSlash(tc.endpoint)},
				profilepolicy.Profile{Name: "work", UserDataDir: filepath.FromSlash(tc.policy)},
			)
			if tc.refused && err == nil {
				t.Fatal("a daemon driving one profile while reporting another was accepted")
			}
			if !tc.refused && err != nil {
				t.Fatalf("refused a matching endpoint: %v", err)
			}
		})
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
