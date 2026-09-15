package mcp

import (
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/siteconsent"
)

// Acceptance 2, the consent half. The site-consent gate is enforced above the
// controller, so it is transport-independent by construction — but "by
// construction" is the claim, and a transport-aware shortcut added later is
// exactly how such a claim stops being true.
//
// Enumerated over the declared transports rather than written for the remote
// one, so a transport that is added and forgotten fails here instead of
// shipping ungated.
func TestSiteConsentIsEnforcedOnEveryTransportIncludingAProviderBackedBrowser(t *testing.T) {
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
