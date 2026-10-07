package mcp

import (
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/siteconsent"
)

func TestSiteConsentDoesNotBranchOnTransport(t *testing.T) {
	for _, transport := range brwidentity.Transports() {
		t.Run(transport, func(t *testing.T) {
			ctrl := &consentController{tabURL: "https://shop.test/cart"}
			srv, guard := newConsentServer(t, ctrl, siteconsent.AdminConfig{})
			srv.SetIdentity(brwidentity.Identity{Transport: transport, Workspace: "fixture-workspace"})

			response := callConsentTool(t, srv, "brw_open", map[string]any{"url": "https://ungranted.test/page"})
			if !strings.Contains(response, `"isError":true`) {
				t.Fatalf("transport %q opened an un-granted origin: %s", transport, response)
			}
			if ctrl.openURL != "" {
				t.Fatalf("transport %q reached the controller with %q despite the refusal", transport, ctrl.openURL)
			}

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

func TestIdentityReportsTheProviderBackedTransport(t *testing.T) {
	srv := New(nil)
	srv.SetIdentity(brwidentity.Identity{Workspace: "fixture-workspace", Transport: brwidentity.TransportOffHostCDP, Mode: "browser-provider"})
	response := callConsentTool(t, srv, "brw_identity", nil)
	if !strings.Contains(response, brwidentity.TransportOffHostCDP) {
		t.Fatalf("brw_identity = %s, want it to name the %s transport", response, brwidentity.TransportOffHostCDP)
	}

	if strings.Contains(response, "user_data_dir") {
		t.Fatalf("brw_identity = %s, want no local profile on a provider-backed daemon", response)
	}
}

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

	for _, capability := range []string{"brw_cookies", "brw_state"} {
		if !strings.Contains(description, capability) {
			t.Errorf("the brw_identity description does not mention %s, whose availability differs by transport", capability)
		}
	}

	for _, crowding := range []string{"brw_upload_file", "brw_downloads", "brw_set_download_path"} {
		if strings.Contains(description, crowding) {
			t.Errorf("the brw_identity description names %s, which makes it a discovery match for searches meant for that tool", crowding)
		}
	}
}
