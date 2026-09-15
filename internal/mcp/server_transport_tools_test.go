package mcp

import (
	"slices"
	"sort"
	"strings"
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
			shown:     []string{"brw_open_incognito", "brw_close_context", "brw_cookies", "brw_state", "brw_downloads", "brw_upload_file", "brw_clipboard"},
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
		{
			// --remote: full CDP against a browser somebody else started. The
			// download tool goes for the same reason it goes on the opt-in
			// lane, and the Manager refuses it there whatever tools/list says.
			name:      "remote cdp has everything direct cdp has except download routing",
			transport: brwidentity.TransportRemoteCDP,
			hidden: []string{
				"brw_group_tabs", "brw_ungroup_tabs", "brw_list_tab_groups",
				"brw_set_download_path",
			},
			shown: []string{
				"brw_open_incognito", "brw_close_context", "brw_cookies", "brw_clipboard",
				"brw_set_geolocation", "brw_authenticate", "brw_state",
				// The browser is on this machine, so a path still names the file
				// the caller meant. Only the ROUTING of downloads is refused.
				"brw_downloads", "brw_upload_file",
			},
		},
		{
			// A plugin-supplied browser is CDP over a socket to another machine.
			// Everything that resolves a path, the clipboard or this host's
			// session-snapshot store on the machine the browser runs on would
			// answer about the wrong one, so it is not advertised; everything
			// else is exactly direct CDP.
			//
			// This is the row that distinguishes the lane from --remote above:
			// both attached to a browser brw did not start, and only this one
			// has to refuse the local filesystem, the clipboard and the store.
			//
			// brw_state is hidden for the stronger reason. Restoring a snapshot
			// is the other sanctioned way to put a session a human signed into
			// HERE into a fresh browser, so leaving it advertised would walk
			// past the profile gates that refuse exactly that.
			name:      "an off-host browser hides this machine's filesystem, clipboard and session store",
			transport: brwidentity.TransportOffHostCDP,
			hidden: []string{
				"brw_downloads", "brw_set_download_path", "brw_upload_file", "brw_clipboard",
				"brw_state",
				"brw_group_tabs", "brw_ungroup_tabs", "brw_list_tab_groups",
			},
			shown: []string{
				"brw_open_incognito", "brw_close_context", "brw_cookies",
				"brw_key_down", "brw_pushstate", "brw_set_geolocation", "brw_authenticate",
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
// filtering the tool it was meant to hide. Every tool must also carry at least
// one requirement, or the row classifies nothing while looking classified.
func TestToolRequirementsNameRealTools(t *testing.T) {
	known := map[string]bool{}
	for _, tool := range tools() {
		name, _ := tool["name"].(string)
		known[name] = true
	}
	for name, reqs := range toolRequirements {
		if !known[name] {
			t.Errorf("toolRequirements names %q, which is not a registered tool", name)
		}
		if len(reqs) == 0 {
			t.Errorf("toolRequirements[%q] carries no requirement, so the row gates nothing", name)
		}
		seen := map[toolRequirement]bool{}
		for _, req := range reqs {
			if _, named := requirementNames[req]; !named {
				t.Errorf("toolRequirements[%q] carries requirement %d, which requirementNames does not name", name, req)
			}
			if seen[req] {
				t.Errorf("toolRequirements[%q] carries %s twice", name, requirementNames[req])
			}
			seen[req] = true
		}
	}
}

// The derived table is what supportedOnTransport reads, so its own shape has to
// hold: every transport it names must be one brwidentity declares — enumerated
// against that closed list rather than spelled out here, so a new transport
// cannot be classified against a typo — and no tool may be excluded everywhere.
func TestTransportUnsupportedNamesKnownTransports(t *testing.T) {
	for name, transports := range transportUnsupported {
		if len(transports) == 0 {
			t.Errorf("transportUnsupported[%q] is empty, so the row filters nothing", name)
		}
		seen := map[string]bool{}
		for _, transport := range transports {
			if !slices.Contains(brwidentity.Transports(), transport) {
				t.Errorf("transportUnsupported[%q] names %q, which is not in %v", name, transport, brwidentity.Transports())
			}
			if seen[transport] {
				t.Errorf("transportUnsupported[%q] names %q twice", name, transport)
			}
			seen[transport] = true
		}
		if len(transports) == len(brwidentity.Transports()) {
			t.Errorf("transportUnsupported[%q] excludes every transport, so the tool can never run anywhere and should not be registered", name)
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
	if len(transports) < 4 {
		t.Fatalf("brwidentity reports %d transports; this test exists to cover all of them", len(transports))
	}
	// Every requirement value in use must have a rule in runnableOn. A new
	// constant defaulting through the switch would read as "no transport can
	// run it", which is at least loud — but a value that reached the default
	// while meaning "everyone can" would hide every tool everywhere, so the
	// enumeration is explicit.
	requirements := map[toolRequirement]bool{}
	for _, reqs := range toolRequirements {
		for _, req := range reqs {
			requirements[req] = true
		}
	}
	for _, req := range []toolRequirement{needsCDPSession, needsBrowserTarget, needsExtensionAPIs, refusedOnSignedInProfile, needsDownloadRouting, needsLocalBrowserHost} {
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
		{"brw_group_tabs", []string{brwidentity.TransportChromeOptIn, brwidentity.TransportDirectCDP, brwidentity.TransportOffHostCDP, brwidentity.TransportRemoteCDP}},
		{"brw_state", []string{brwidentity.TransportChromeOptIn, brwidentity.TransportExtensionBridge, brwidentity.TransportOffHostCDP}},
		{"brw_set_download_path", []string{brwidentity.TransportChromeOptIn, brwidentity.TransportExtensionBridge, brwidentity.TransportOffHostCDP, brwidentity.TransportRemoteCDP}},
		// The clipboard row is what a single-requirement table could not state:
		// the bridge has no browser target, the off-host lane has one and would
		// answer about the provider's machine. Two reasons, two lanes.
		{"brw_clipboard", []string{brwidentity.TransportExtensionBridge, brwidentity.TransportOffHostCDP}},
		// Refused only where the browser is on another machine. --remote is NOT
		// on this list: a loopback endpoint shares this disk, and refusing it
		// there would take away a capability that works.
		{"brw_downloads", []string{brwidentity.TransportOffHostCDP}},
		{"brw_upload_file", []string{brwidentity.TransportOffHostCDP}},
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

// Every declared transport has to actually be exercised by the filter, or a
// transport added to brwidentity and nowhere else advertises a surface nobody
// checked.
func TestEveryDeclaredTransportProducesACatalogue(t *testing.T) {
	for _, transport := range brwidentity.Transports() {
		s := NewWithToolProfile(nil, "all")
		s.SetIdentity(brwidentity.Identity{Transport: transport})
		got := advertisedNames(s)
		if len(got) == 0 {
			t.Fatalf("transport %q advertises nothing", transport)
		}
		for name, transports := range transportUnsupported {
			if slices.Contains(transports, transport) && got[name] {
				t.Errorf("transport %q advertises %s, which can never succeed there", transport, name)
			}
		}
	}
}

// transportOnlyClaims maps a phrase an agent reads as "this tool runs on
// exactly one transport" to the transport it names. Lowercased, because the
// descriptions shout some of them and not others.
func transportOnlyClaims() map[string]string {
	return map[string]string{
		"direct-cdp transport only":        brwidentity.TransportDirectCDP,
		"direct cdp transport only":        brwidentity.TransportDirectCDP,
		"direct-cdp only":                  brwidentity.TransportDirectCDP,
		"direct cdp only":                  brwidentity.TransportDirectCDP,
		"extension-bridge transport only":  brwidentity.TransportExtensionBridge,
		"extension bridge transport only":  brwidentity.TransportExtensionBridge,
		"extension-bridge only":            brwidentity.TransportExtensionBridge,
		"extension bridge only":            brwidentity.TransportExtensionBridge,
		"remote-cdp transport only":        brwidentity.TransportRemoteCDP,
		"remote-cdp only":                  brwidentity.TransportRemoteCDP,
		"off-host-cdp transport only":      brwidentity.TransportOffHostCDP,
		"off-host-cdp only":                brwidentity.TransportOffHostCDP,
		"chrome-opt-in-cdp transport only": brwidentity.TransportChromeOptIn,
		"chrome-opt-in-cdp only":           brwidentity.TransportChromeOptIn,
	}
}

// A description is the only thing an agent has to decide what its lane can do,
// and a tool handed to it on one transport that says it runs on a different one
// is worse than no sentence at all: it reads as "you are on the wrong daemon"
// for a call that would have worked.
//
// This is the check the branch that added a third transport needed. Three tools
// said "direct-CDP only" while being advertised on remote-cdp, and four said
// "works on both transports" when there were three, because nothing compared
// the prose against the table that decides what is advertised.
func TestNoAdvertisedToolClaimsADifferentTransport(t *testing.T) {
	for _, transport := range brwidentity.Transports() {
		t.Run(transport, func(t *testing.T) {
			s := NewWithToolProfile(nil, "all")
			s.SetIdentity(brwidentity.Identity{Transport: transport})
			for _, tool := range s.advertisedTools() {
				name, _ := tool["name"].(string)
				description, _ := tool["description"].(string)
				lowered := strings.ToLower(description)
				for phrase, claimed := range transportOnlyClaims() {
					if strings.Contains(lowered, phrase) && claimed != transport {
						t.Errorf("%s is advertised on %q and its description says %q; make the sentence true or drop it", name, transport, phrase)
					}
				}
				// "both transports" was written when there were two.
				for _, stale := range []string{"both transports", "either transport", "the two transports"} {
					if strings.Contains(lowered, stale) {
						t.Errorf("%s says %q; brw advertises %d transports", name, stale, len(brwidentity.Transports()))
					}
				}
			}
		})
	}
}

// The converse: a tool that really does run on exactly one transport should say
// so, and the sentence has to name the transport the table agrees with. This is
// what stops the fix above being "delete every sentence".
func TestATransportOnlyClaimMatchesTheTable(t *testing.T) {
	for _, tool := range tools() {
		name, _ := tool["name"].(string)
		description, _ := tool["description"].(string)
		lowered := strings.ToLower(description)
		for phrase, claimed := range transportOnlyClaims() {
			if !strings.Contains(lowered, phrase) {
				continue
			}
			var advertised []string
			for _, transport := range brwidentity.Transports() {
				if !slices.Contains(transportUnsupported[name], transport) {
					advertised = append(advertised, transport)
				}
			}
			if !slices.Equal(advertised, []string{claimed}) {
				t.Errorf("%s says %q but is advertised on %v", name, phrase, advertised)
			}
		}
	}
}
