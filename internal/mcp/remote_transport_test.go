package mcp

import (
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/siteconsent"
)

// The site-consent gate is enforced above the controller, so it is
// transport-independent by construction — but "by construction" is the claim,
// and a transport-aware shortcut added later is exactly how such a claim stops
// being true.
//
// What this proves is that the gate does not BRANCH on transport: it drives a
// fake controller stamped with each declared transport in turn, so a transport
// that is added and forgotten fails here instead of shipping ungated. It is
// deliberately NOT the acceptance evidence for "enforced against the remote
// target" — there is no provider-backed browser anywhere in it, and an earlier
// name claiming otherwise read as if there were.
// TestSiteConsentIsEnforcedAgainstAProviderBackedBrowser is that evidence.
func TestSiteConsentDoesNotBranchOnTransport(t *testing.T) {
	for _, transport := range brwidentity.Transports() {
		t.Run(transport, func(t *testing.T) {
			ctrl := &consentController{tabURL: "https://shop.test/cart"}
			srv, guard := newConsentServer(t, ctrl, siteconsent.AdminConfig{})
			srv.SetIdentity(brwidentity.Identity{Transport: transport, Workspace: "fixture-workspace"})

			// read scope, on the navigation.
			response := callConsentTool(t, srv, "brw_open", map[string]any{"url": "https://ungranted.test/page"})
			if !strings.Contains(response, `"isError":true`) {
				t.Fatalf("transport %q opened an un-granted origin: %s", transport, response)
			}
			if ctrl.openURL != "" {
				t.Fatalf("transport %q reached the controller with %q despite the refusal", transport, ctrl.openURL)
			}

			// act scope, against the tab's live origin, with read already given.
			if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
				t.Fatal(err)
			}
			response = callConsentTool(t, srv, "brw_click", map[string]any{"ref": "e1"})
			if !strings.Contains(response, `"isError":true`) {
				t.Fatalf("transport %q acted on a read-only grant: %s", transport, response)
			}
			if ctrl.clicked {
				t.Fatalf("transport %q ran the click despite the refusal", transport)
			}

			// And a full grant still works, so the gate is a gate rather than a
			// transport-wide refusal that would pass the assertions above.
			if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
				t.Fatal(err)
			}
			response = callConsentTool(t, srv, "brw_click", map[string]any{"ref": "e1"})
			if strings.Contains(response, `"isError":true`) {
				t.Fatalf("transport %q refused a granted action: %s", transport, response)
			}
			if !ctrl.clicked {
				t.Fatalf("transport %q never dispatched the granted action", transport)
			}
		})
	}
}

// An agent has to be able to tell it is driving somebody else's browser, or it
// cannot avoid asking that browser for this machine's files. brw_identity is
// the surface that says so.
func TestIdentityReportsTheProviderBackedTransport(t *testing.T) {
	srv := New(nil)
	srv.SetIdentity(brwidentity.Identity{Workspace: "fixture-workspace", Transport: brwidentity.TransportOffHostCDP, Mode: "browser-provider"})
	response := callConsentTool(t, srv, "brw_identity", nil)
	if !strings.Contains(response, brwidentity.TransportOffHostCDP) {
		t.Fatalf("brw_identity = %s, want it to name the %s transport", response, brwidentity.TransportOffHostCDP)
	}
	// A provider-backed daemon has no local profile, and saying it had one
	// would be the claim the whole capability model refuses to make.
	if strings.Contains(response, "user_data_dir") {
		t.Fatalf("brw_identity = %s, want no local profile on a provider-backed daemon", response)
	}
}

// brw_identity is the surface an agent is told to call to learn what its lane
// can do, so its DESCRIPTION is the sentence most likely to be acted on. It
// enumerated the transport set as two values and stated capability rules that a
// third transport falsified, and the response assertion above never looked at
// it.
//
// Keyed on brwidentity.Transports() rather than on a list written out here, so
// a fourth transport fails until somebody says what it can do.
func TestTheIdentityDescriptionNamesEveryTransport(t *testing.T) {
	var description string
	for _, tool := range tools() {
		if name, _ := tool["name"].(string); name == "brw_identity" {
			description, _ = tool["description"].(string)
		}
	}
	if description == "" {
		t.Fatal("brw_identity has no description")
	}
	for _, transport := range brwidentity.Transports() {
		if !strings.Contains(description, transport) {
			t.Errorf("the brw_identity description does not name the %q transport, so an agent on it cannot learn what its lane can do", transport)
		}
	}
	// The rule that made the old sentence wrong, stated the right way round.
	// Naming a tool as direct-cdp-only while advertising it on remote-cdp is
	// what an agent acts on when it decides not to try.
	for _, capability := range []string{"brw_cookies", "brw_state"} {
		if !strings.Contains(description, capability) {
			t.Errorf("the brw_identity description does not mention %s, whose availability differs by transport", capability)
		}
	}
	// It names as few tools as it can. brw_tools scores a description against
	// the query, so a tool named here makes brw_identity a match for searches
	// meant for that tool — which is how "upload a file" started matching
	// brw_identity (see TestSearchDoesNotMatchInsideLongerWords). The
	// filesystem-bound tools are therefore described by what they do.
	for _, crowding := range []string{"brw_upload_file", "brw_downloads", "brw_set_download_path"} {
		if strings.Contains(description, crowding) {
			t.Errorf("the brw_identity description names %s, which makes it a discovery match for searches meant for that tool", crowding)
		}
	}
}
