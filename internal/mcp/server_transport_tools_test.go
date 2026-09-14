package mcp

import (
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
			hidden:    []string{"brw_open_incognito", "brw_close_context", "brw_cookies"},
			shown:     []string{"brw_group_tabs", "brw_ungroup_tabs", "brw_list_tab_groups"},
		},
		{
			name:      "direct cdp hides tab groups",
			transport: brwidentity.TransportDirectCDP,
			hidden:    []string{"brw_group_tabs", "brw_ungroup_tabs", "brw_list_tab_groups"},
			shown:     []string{"brw_open_incognito", "brw_close_context", "brw_cookies"},
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

// Every entry in the table must name a real tool, or a rename silently stops
// filtering the tool it was meant to hide.
func TestTransportUnsupportedNamesRealTools(t *testing.T) {
	known := map[string]bool{}
	for _, tool := range tools() {
		name, _ := tool["name"].(string)
		known[name] = true
	}
	for name, transport := range transportUnsupported {
		if !known[name] {
			t.Errorf("transportUnsupported names %q, which is not a registered tool", name)
		}
		if transport != brwidentity.TransportExtensionBridge && transport != brwidentity.TransportDirectCDP {
			t.Errorf("transportUnsupported[%q] = %q, not a known transport", name, transport)
		}
	}
}
