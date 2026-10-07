package mcp

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

func advertisedNames(s *Server) map[string]bool {
	names := map[string]bool{}
	for _, t := range s.advertisedTools() {
		name, _ := t["name"].(string)
		names[name] = true
	}
	return names
}

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

			name:      "chrome opt-in has cookies and incognito but not tab groups or state",
			transport: brwidentity.TransportChromeOptIn,
			hidden: []string{
				"brw_group_tabs", "brw_ungroup_tabs", "brw_list_tab_groups", "brw_state",

				"brw_set_download_path",
			},
			shown: []string{
				"brw_open_incognito", "brw_close_context", "brw_cookies", "brw_clipboard",
				"brw_set_geolocation", "brw_authenticate",
			},
		},
		{

			name:      "remote cdp has everything direct cdp has except download routing",
			transport: brwidentity.TransportRemoteCDP,
			hidden: []string{
				"brw_group_tabs", "brw_ungroup_tabs", "brw_list_tab_groups",
				"brw_set_download_path",
			},
			shown: []string{
				"brw_open_incognito", "brw_close_context", "brw_cookies", "brw_clipboard",
				"brw_set_geolocation", "brw_authenticate", "brw_state",

				"brw_downloads", "brw_upload_file",
			},
		},
		{

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

func TestAdvertisedToolsUnknownTransportKeepsEverything(t *testing.T) {
	s := NewWithToolProfile(nil, "all")
	got := advertisedNames(s)
	for name := range transportUnsupported {
		if !got[name] {
			t.Errorf("unknown transport dropped %s; it should advertise the full surface", name)
		}
	}
}

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

func TestEveryTransportClassifiesEveryCapabilityGatedTool(t *testing.T) {
	transports := brwidentity.Transports()
	if len(transports) < 4 {
		t.Fatalf("brwidentity reports %d transports; this test exists to cover all of them", len(transports))
	}

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

		{"brw_clipboard", []string{brwidentity.TransportExtensionBridge, brwidentity.TransportOffHostCDP}},

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

				for _, stale := range []string{"both transports", "either transport", "the two transports"} {
					if strings.Contains(lowered, stale) {
						t.Errorf("%s says %q; brw advertises %d transports", name, stale, len(brwidentity.Transports()))
					}
				}
			}
		})
	}
}

func TestATransportOnlyClaimMatchesTheTable(t *testing.T) {
	for _, tool := range tools() {
		name, _ := tool["name"].(string)
		description, _ := tool["description"].(string)
		lowered := strings.ToLower(description)
		for phrase, claimed := range transportOnlyClaims() {
			if !strings.Contains(lowered, phrase) {
				continue
			}
			advertised := advertisedTransports(name)
			if !slices.Equal(advertised, []string{claimed}) {
				t.Errorf("%s says %q but is advertised on %v", name, phrase, advertised)
			}
		}
	}
}

var requirementNames = map[toolRequirement]string{
	needsCDPSession:          "needsCDPSession",
	needsBrowserTarget:       "needsBrowserTarget",
	needsExtensionAPIs:       "needsExtensionAPIs",
	refusedOnSignedInProfile: "refusedOnSignedInProfile",
	needsDownloadRouting:     "needsDownloadRouting",
	needsLocalBrowserHost:    "needsLocalBrowserHost",
}
