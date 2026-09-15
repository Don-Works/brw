package mcp

import (
	"sort"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

// advertisedNames is the set of tool names tools/list would return.
func advertisedNames(s *Server) map[string]bool {
	names := map[string]bool{}
	for _, t := range s.advertisedTools() {
		name, _ := t["name"].(string)
		names[name] = true
	}
	return names
}

// A tool whose controller method returns Err*Unsupported unconditionally must
// not appear in tools/list on that transport. Advertising it costs an agent a
// round trip and a recovery path for a capability that was never there.
func TestAdvertisedToolsDropTransportUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport string
		hidden    []string
		shown     []string
	}{
		{
			name:      "extension bridge hides incognito and cookies",
			transport: brwidentity.TransportExtensionBridge,
			hidden:    []string{"brw_open_incognito", "brw_close_context", "brw_cookies", "brw_state"},
			shown:     []string{"brw_group_tabs", "brw_ungroup_tabs", "brw_list_tab_groups"},
		},
		{
			name:      "direct cdp hides tab groups",
			transport: brwidentity.TransportDirectCDP,
			hidden:    []string{"brw_group_tabs", "brw_ungroup_tabs", "brw_list_tab_groups"},
			shown:     []string{"brw_open_incognito", "brw_close_context", "brw_cookies", "brw_state"},
		},
		{
			// The lane the whole opt-in exists for: the bridge's incognito and
			// cookie restrictions are gone, because this is real browser-target
			// CDP, while tab groups are still an extension API and brw_state is
			// still refused on a browser its user is signed into.
			name:      "chrome opt-in has cookies and incognito but not tab groups or state",
			transport: brwidentity.TransportChromeOptIn,
			hidden: []string{
				"brw_group_tabs", "brw_ungroup_tabs", "brw_list_tab_groups", "brw_state",
				// The one page-environment tool the lane does NOT get: pointing
				// downloads at brw's staging directory is browser-context-wide,
				// so on this lane it would move the user's own files.
				"brw_set_download_path",
			},
			shown: []string{
				"brw_open_incognito", "brw_close_context", "brw_cookies", "brw_clipboard",
				"brw_set_geolocation", "brw_authenticate",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, profile := range []string{"all", "core", "minimal", "auto", "typo-profile"} {
				s := NewWithToolProfile(nil, profile)
				s.SetIdentity(brwidentity.Identity{Transport: tc.transport})
				got := advertisedNames(s)
				for _, name := range tc.hidden {
					if got[name] {
						t.Errorf("profile %q transport %q: advertised %s, which always fails there", profile, tc.transport, name)
					}
				}
				// The reverse transport's tools must still be advertised where
				// they work, and only when the profile includes them at all.
				if profile == "all" || profile == "typo-profile" {
					for _, name := range tc.shown {
						if !got[name] {
							t.Errorf("profile %q transport %q: dropped %s, which works there", profile, tc.transport, name)
						}
					}
				}
			}
		})
	}
}

// A daemon that never recorded its identity advertises everything: hiding a
// tool because the transport is merely unknown is a worse failure than
// advertising one that errors.
func TestAdvertisedToolsUnknownTransportKeepsEverything(t *testing.T) {
	s := NewWithToolProfile(nil, "all")
	got := advertisedNames(s)
	for name := range transportUnsupported {
		if !got[name] {
			t.Errorf("unknown transport dropped %s; it should advertise the full surface", name)
		}
	}
}

// Every classified tool must name a real tool, or a rename silently stops
// filtering the tool it was meant to hide.
func TestToolRequirementsNameRealTools(t *testing.T) {
	known := map[string]bool{}
	for _, tool := range tools() {
		name, _ := tool["name"].(string)
		known[name] = true
	}
	for name := range toolRequirements {
		if !known[name] {
			t.Errorf("toolRequirements names %q, which is not a registered tool", name)
		}
	}
}

// The table is the gate on what a lane advertises, so every member of the
// domain it gates has to be classified. The domain is every capability-gated
// tool crossed with every transport brw can report: a transport added to
// brwidentity without properties, or a requirement added without a rule, would
// otherwise quietly resolve to "available" and the tool would be advertised on
// a lane that always fails it.
func TestEveryTransportClassifiesEveryCapabilityGatedTool(t *testing.T) {
	transports := brwidentity.Transports()
	if len(transports) < 3 {
		t.Fatalf("brwidentity reports %d transports; this test exists to cover all of them", len(transports))
	}
	// Every requirement value in use must have a rule in runnableOn. A new
	// constant defaulting through the switch would read as "no transport can
	// run it", which is at least loud — but a value that reached the default
	// while meaning "everyone can" would hide every tool everywhere, so the
	// enumeration is explicit.
	requirements := map[toolRequirement]bool{}
	for _, req := range toolRequirements {
		requirements[req] = true
	}
	for _, req := range []toolRequirement{needsCDPSession, needsBrowserTarget, needsExtensionAPIs, refusedOnSignedInProfile, needsDownloadRouting} {
		delete(requirements, req)
	}
	if len(requirements) != 0 {
		t.Fatalf("toolRequirements uses %d requirement value(s) this test does not enumerate", len(requirements))
	}

	for _, transport := range transports {
		caps, known := brwidentity.Capabilities(transport)
		if !known {
			t.Errorf("transport %q has no declared capabilities, so every tool's availability on it is undefined", transport)
			continue
		}
		// A lane that declares nothing would silently refuse every gated tool
		// while looking classified.
		if !caps.CDPSession && !caps.BrowserTarget && !caps.ExtensionAPIs {
			t.Errorf("transport %q declares no capability at all; it cannot carry any capability-gated tool", transport)
		}
		for tool := range toolRequirements {
			s := NewWithToolProfile(nil, "all")
			s.SetIdentity(brwidentity.Identity{Transport: transport})
			advertised := advertisedNames(s)[tool]
			if advertised == unsupportedOn(tool, transport) {
				t.Errorf("tool %s on transport %s: advertised=%v but unsupported=%v; tools/list and the capability table disagree",
					tool, transport, advertised, unsupportedOn(tool, transport))
			}
		}
	}
}

// The derived table must reach the answers the three lanes are documented with
// in docs/install.md. Without this the derivation could be self-consistently
// wrong: a rule that inverted needsExtensionAPIs would still satisfy the
// enumeration above.
func TestDerivedTableMatchesTheDocumentedLanes(t *testing.T) {
	for _, tc := range []struct {
		tool string
		want []string
	}{
		{"brw_cookies", []string{brwidentity.TransportExtensionBridge}},
		{"brw_open_incognito", []string{brwidentity.TransportExtensionBridge}},
		{"brw_set_geolocation", []string{brwidentity.TransportExtensionBridge}},
		{"brw_group_tabs", []string{brwidentity.TransportChromeOptIn, brwidentity.TransportDirectCDP}},
		{"brw_state", []string{brwidentity.TransportChromeOptIn, brwidentity.TransportExtensionBridge}},
		{"brw_set_download_path", []string{brwidentity.TransportChromeOptIn, brwidentity.TransportExtensionBridge}},
	} {
		got := append([]string(nil), transportUnsupported[tc.tool]...)
		sort.Strings(got)
		sort.Strings(tc.want)
		if len(got) != len(tc.want) {
			t.Fatalf("%s unsupported on %v, want %v", tc.tool, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s unsupported on %v, want %v", tc.tool, got, tc.want)
			}
		}
	}
}
