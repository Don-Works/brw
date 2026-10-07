package brwidentity

import (
	"slices"
	"testing"
)

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

	if got := remote.Mismatches(Identity{}); len(got) != 0 {
		t.Fatalf("an unpinned proxy refused a provider-backed daemon: %v", got)
	}
}

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
