package brwidentity

import (
	"slices"
	"testing"
)

// Acceptance 2, the identity-guard half. A proxy pinned to a workspace profile
// must NOT accept a daemon driving somebody else's browser, and it must not
// accept it by way of the empty fields such a daemon reports.
//
// The identity here is the one a provider-backed brwd actually builds, which is
// the correction: an earlier version of this test pinned Workspace "client-a"
// on it, and no provider-backed daemon can report that — cmd/brwd refuses
// --profile and --workspace with a provider, so the profile-policy block that
// fills those fields never runs and the identity carries Mode and Transport and
// nothing else. That made the test a property of Mismatches rather than of what
// this daemon reports, and it locked the half that does not matter: it asserted
// a workspace pin SUCCEEDS, in a shape where a workspace can never be set.
//
// cmd/brwd's TestTheIdentityAProviderLaunchReportsCarriesNoProfile builds it
// from resolveIdentity and asserts the same refusals; this one states the
// property of Mismatches those depend on.
func TestNoProfileOrWorkspacePinIsSatisfiedByAProviderBackedDaemon(t *testing.T) {
	remote := Identity{
		Transport: TransportOffHostCDP,
		Mode:      "browser-provider",
	}
	if remote.Empty() {
		t.Fatal("a provider-backed daemon must not read as having no identity at all")
	}
	expected := Identity{
		Workspace:        "client-a",
		Profile:          "client-a-chrome",
		UserDataDir:      "/profiles/client-a",
		ProfileDirectory: "Profile 1",
	}
	mismatches := remote.Mismatches(expected)
	if len(mismatches) != 4 {
		t.Fatalf("mismatches = %v, want the workspace, the profile, the user data dir and the profile directory", mismatches)
	}
	for _, field := range []string{"workspace", "profile", "user_data_dir", "profile_directory"} {
		if !slices.ContainsFunc(mismatches, func(m string) bool { return len(m) > len(field) && m[:len(field)] == field }) {
			t.Errorf("mismatches = %v, want one naming %q", mismatches, field)
		}
	}
	// Each pin alone is enough to refuse, so a proxy that pins only a workspace
	// is not talked past by a daemon that reports none.
	for _, pin := range []Identity{
		{Workspace: "client-a"},
		{Profile: "client-a-chrome"},
		{UserDataDir: "/profiles/client-a"},
		{ProfileDirectory: "Profile 1"},
	} {
		if got := remote.Mismatches(pin); len(got) != 1 {
			t.Errorf("pin %+v against a provider-backed daemon = %v, want exactly one mismatch", pin, got)
		}
	}
	// An unpinned proxy still accepts it: the guard refuses a claim it cannot
	// satisfy, not the transport itself.
	if got := remote.Mismatches(Identity{}); len(got) != 0 {
		t.Fatalf("an unpinned proxy refused a provider-backed daemon: %v", got)
	}
}

// Transport is adopted from an upstream rather than asserted, so it must stay
// out of the comparison for the remote one too — otherwise a proxy in front of
// a provider-backed daemon would reject its own upstream.
func TestTheOffHostTransportIsNotItselfAMismatch(t *testing.T) {
	upstream := Identity{Workspace: "client-a", Profile: "p", Transport: TransportOffHostCDP, Headless: true}
	if got := upstream.Mismatches(Identity{Workspace: "client-a", Profile: "p"}); len(got) != 0 {
		t.Fatalf("mismatches = %v, want none", got)
	}
	transportOnly := Identity{Transport: TransportOffHostCDP}
	if transportOnly.Empty() {
		t.Fatal("a daemon that reported only its transport must not read as having no identity; /health omits an empty one entirely")
	}
}

// Transports() is what tables keyed by transport enumerate against. A constant
// that exists and is missing from the list is a transport every such table
// silently fails to classify.
func TestTransportsListsEveryTransportConstant(t *testing.T) {
	want := []string{TransportChromeOptIn, TransportDirectCDP, TransportExtensionBridge, TransportOffHostCDP, TransportRemoteCDP}
	got := Transports()
	if !slices.Equal(got, want) {
		t.Fatalf("Transports() = %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, transport := range got {
		if transport == "" {
			t.Error("Transports() contains an empty name; an unset transport means \"unknown\", not a transport")
		}
		if seen[transport] {
			t.Errorf("Transports() lists %q twice", transport)
		}
		seen[transport] = true
	}
}
