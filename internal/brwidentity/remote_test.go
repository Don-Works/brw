package brwidentity

import (
	"slices"
	"testing"
)

// Acceptance 2, the identity-guard half. The guard is unchanged code on a
// remote target, and "unchanged" is the thing worth pinning: a proxy pinned to
// a workspace profile must NOT accept a daemon driving somebody else's browser,
// and it must not accept it by way of the empty fields such a daemon reports.
//
// A provider-backed daemon has no local profile. Every field that names one is
// therefore empty, and an expectation that names one fails — which is the guard
// doing exactly what it does for any other mismatch, with no special case.
func TestAProfilePinIsNotSatisfiedByAProviderBackedDaemon(t *testing.T) {
	remote := Identity{
		Workspace: "client-a",
		Transport: TransportOffHostCDP,
		Mode:      "browser-provider",
	}
	expected := Identity{
		Workspace:        "client-a",
		Profile:          "client-a-chrome",
		UserDataDir:      "/profiles/client-a",
		ProfileDirectory: "Profile 1",
	}
	mismatches := remote.Mismatches(expected)
	if len(mismatches) != 3 {
		t.Fatalf("mismatches = %v, want the profile, the user data dir and the profile directory", mismatches)
	}
	for _, field := range []string{"profile", "user_data_dir", "profile_directory"} {
		if !slices.ContainsFunc(mismatches, func(m string) bool { return len(m) > len(field) && m[:len(field)] == field }) {
			t.Errorf("mismatches = %v, want one naming %q", mismatches, field)
		}
	}
	// The workspace binding itself still matches, so the refusal is about the
	// profile rather than about the transport being unfamiliar.
	if got := remote.Mismatches(Identity{Workspace: "client-a"}); len(got) != 0 {
		t.Fatalf("a workspace-only pin against a provider-backed daemon = %v, want no mismatch", got)
	}
	if got := remote.Mismatches(Identity{Workspace: "client-b"}); len(got) != 1 {
		t.Fatalf("a wrong workspace = %v, want exactly one mismatch", got)
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
