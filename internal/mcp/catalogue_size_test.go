package mcp

import (
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

func TestNarrowProfilesAreTheSameSizeOnEveryTransport(t *testing.T) {
	for _, profile := range []string{"core", "minimal", autoProfile} {
		t.Run(profile, func(t *testing.T) {
			unfiltered := len((&Server{toolProfile: profile}).advertisedTools())
			if unfiltered == 0 {
				t.Fatalf("profile %q advertises nothing, so this test would pass by vacuum", profile)
			}
			for _, transport := range []string{brwidentity.TransportDirectCDP, brwidentity.TransportExtensionBridge} {
				server := &Server{toolProfile: profile, identity: brwidentity.Identity{Transport: transport}}
				if got := len(server.advertisedTools()); got != unfiltered {
					t.Errorf("profile %q advertises %d tools on %s and %d unfiltered; the docs say the narrow profiles are the same size everywhere", profile, got, transport, unfiltered)
				}
			}
		})
	}
}
