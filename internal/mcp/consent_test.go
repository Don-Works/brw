package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/snapshot"
)

// consentController is a recordingController that also reports an open tab, so
// the act-scope gate has a live page origin to check against, and records
// whether the underlying action ran.
type consentController struct {
	recordingController
	tabURL      string
	clicked     bool
	findResults []snapshot.Element
}

func (c *consentController) ListTabs(context.Context) ([]browser.Tab, error) {
	return []browser.Tab{{ID: "tab1", URL: c.tabURL, Active: true}}, nil
}

func (c *consentController) Click(ctx context.Context, ref string) (browser.ActionResult, error) {
	c.clicked = true
	return c.recordingController.Click(ctx, ref)
}

func (c *consentController) Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error) {
	return snapshot.FindResult{Elements: c.findResults}, nil
}

// fixtureConsentKey is an obviously fabricated MAC key for tests.
var fixtureConsentKey = []byte("fixture-mcp-consent-key-abcdefghi")

func newConsentServer(t *testing.T, ctrl browser.Controller, admin siteconsent.AdminConfig) (*Server, *siteconsent.Guard) {
	t.Helper()
	if err := admin.Normalize(); err != nil {
		t.Fatal(err)
	}
	store, err := siteconsent.NewStoreWithKey(filepath.Join(t.TempDir(), "site-grants.json"), fixtureConsentKey)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := siteconsent.NewGuard(store, admin)
	if err != nil {
		t.Fatal(err)
	}
	guard.SetGrantor("fixture-user")
	srv := New(ctrl)
	srv.SetSiteConsent(guard)
	return srv, guard
}

func callConsentTool(t *testing.T, srv *Server, tool string, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, rpcErr := srv.callTool(context.Background(), tool, raw)
	if rpcErr != nil {
		t.Fatalf("%s rpc error: %+v", tool, rpcErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestConsentRefusesUngrantedNavigationAndNamesTheScope proves the gate runs
// before dispatch: the controller must not have been asked to open anything.
func TestConsentRefusesUngrantedNavigationAndNamesTheScope(t *testing.T) {
	ctrl := &consentController{tabURL: "https://start.test/"}
	srv, guard := newConsentServer(t, ctrl, siteconsent.AdminConfig{})

	response := callConsentTool(t, srv, "brw_open", map[string]any{"url": "https://ungranted.test/page"})
	if !strings.Contains(response, `"isError":true`) {
		t.Fatalf("an un-granted origin was opened: %s", response)
	}
	if !strings.Contains(response, "https://ungranted.test") || !strings.Contains(response, "read") {
		t.Fatalf("the refusal must name the origin and the missing scope: %s", response)
	}
	if ctrl.openURL != "" {
		t.Fatalf("the controller opened %q despite the refusal", ctrl.openURL)
	}

	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://ungranted.test", Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	response = callConsentTool(t, srv, "brw_open", map[string]any{"url": "https://ungranted.test/page"})
	if strings.Contains(response, `"isError":true`) {
		t.Fatalf("a granted origin was still refused: %s", response)
	}
	if ctrl.openURL != "https://ungranted.test/page" {
		t.Fatalf("the controller opened %q after the grant", ctrl.openURL)
	}
}

// TestConsentGatesEveryNavigationEntrypoint keeps the table honest: a new
// URL-opening tool that is not in consentRules shows up here as a pass when it
// should be a refusal.
func TestConsentGatesEveryNavigationEntrypoint(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"brw_open", map[string]any{"url": "https://ungranted.test/x"}},
		{"brw_open_incognito", map[string]any{"url": "https://ungranted.test/x"}},
		{"brw_navigate_to", map[string]any{"url": "https://ungranted.test/x"}},
		{"brw_read_url", map[string]any{"url": "https://ungranted.test/x"}},
		{"brw_replay_request", map[string]any{"method": "GET", "url": "https://ungranted.test/x"}},
		{"brw_upload_file", map[string]any{"query": "file", "url": "https://ungranted.test/x"}},
		{"brw_plan", map[string]any{"steps": []any{map[string]any{"action": "open", "url": "https://ungranted.test/x"}}}},
		{"brw_batch", map[string]any{"steps": []any{map[string]any{"action": "open", "url": "https://ungranted.test/x"}}}},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			ctrl := &consentController{tabURL: "https://start.test/"}
			srv, _ := newConsentServer(t, ctrl, siteconsent.AdminConfig{})
			response := callConsentTool(t, srv, c.tool, c.args)
			if !strings.Contains(response, `"isError":true`) {
				t.Fatalf("%s reached an un-granted origin: %s", c.tool, response)
			}
			if !strings.Contains(response, "https://ungranted.test") {
				t.Fatalf("%s refusal does not name the origin: %s", c.tool, response)
			}
		})
	}
}

// TestConsentActScopeUsesTheLivePageOrigin proves act is checked against the tab
// the action lands on, not the URL the agent last asked for, and that a read
// grant is not enough to act.
func TestConsentActScopeUsesTheLivePageOrigin(t *testing.T) {
	ctrl := &consentController{tabURL: "https://shop.test/cart"}
	srv, guard := newConsentServer(t, ctrl, siteconsent.AdminConfig{})

	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	response := callConsentTool(t, srv, "brw_click", map[string]any{"ref": "e1"})
	if !strings.Contains(response, `"isError":true`) {
		t.Fatalf("a read grant authorised an action: %s", response)
	}
	if ctrl.clicked {
		t.Fatal("the click ran despite the refusal")
	}
	if !strings.Contains(response, "https://shop.test") || !strings.Contains(response, "act") {
		t.Fatalf("the refusal must name the live origin and the act scope: %s", response)
	}

	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	response = callConsentTool(t, srv, "brw_click", map[string]any{"ref": "e1"})
	if strings.Contains(response, `"isError":true`) {
		t.Fatalf("an act grant did not authorise the click: %s", response)
	}
	if !ctrl.clicked {
		t.Fatal("the click never reached the controller")
	}
}

// TestConsentActFailsClosedWhenTheOriginCannotBeResolved proves the gate does not
// degrade to "allow" when the transport cannot say where the tab is.
func TestConsentActFailsClosedWhenTheOriginCannotBeResolved(t *testing.T) {
	ctrl := &recordingController{} // ListTabs returns no tabs at all
	srv, _ := newConsentServer(t, ctrl, siteconsent.AdminConfig{})
	response := callConsentTool(t, srv, "brw_click", map[string]any{"ref": "e1"})
	if !strings.Contains(response, `"isError":true`) {
		t.Fatalf("an unresolvable origin was allowed to act: %s", response)
	}
	if !strings.Contains(response, "no active tab") {
		t.Fatalf("the refusal must say why it could not decide: %s", response)
	}
}

// TestConsentRevocationAppliesToTheNextCall is the no-restart half of acceptance
// criterion 1, driven through the MCP surface.
func TestConsentRevocationAppliesToTheNextCall(t *testing.T) {
	ctrl := &consentController{tabURL: "https://shop.test/cart"}
	srv, guard := newConsentServer(t, ctrl, siteconsent.AdminConfig{})
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	if response := callConsentTool(t, srv, "brw_click", map[string]any{"ref": "e1"}); strings.Contains(response, `"isError":true`) {
		t.Fatalf("granted click refused: %s", response)
	}
	if _, err := guard.Revoke("https://shop.test", "", "fixture-user"); err != nil {
		t.Fatal(err)
	}
	ctrl.clicked = false
	if response := callConsentTool(t, srv, "brw_click", map[string]any{"ref": "e1"}); !strings.Contains(response, `"isError":true`) {
		t.Fatalf("the click still ran after revocation: %s", response)
	}
	if ctrl.clicked {
		t.Fatal("the click reached the controller after revocation")
	}
}

// TestHighRiskActionRefusedNonInteractively is acceptance criterion 4 at the tool
// surface: a purchase-shaped click is refused, not auto-approved, and an
// ordinary one is untouched.
func TestHighRiskActionRefusedNonInteractively(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		args   map[string]any
		refuse bool
	}{
		{name: "click_text on a purchase control", tool: "brw_click_text", args: map[string]any{"text": "Place order"}, refuse: true},
		{name: "click_text on an ordinary control", tool: "brw_click_text", args: map[string]any{"text": "Show more"}, refuse: false},
		{name: "fill a card number field", tool: "brw_fill", args: map[string]any{"query": "Card number", "text": "4111"}, refuse: true},
		{name: "fill an ordinary field", tool: "brw_fill", args: map[string]any{"query": "Search", "text": "shoes"}, refuse: false},
		{name: "batch step that publishes", tool: "brw_batch", args: map[string]any{"steps": []any{map[string]any{"action": "click_text", "text": "Publish"}}}, refuse: true},
		{name: "batch step that scrolls", tool: "brw_batch", args: map[string]any{"steps": []any{map[string]any{"action": "scroll", "value": "down"}}}, refuse: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctrl := &consentController{tabURL: "https://shop.test/cart"}
			srv, guard := newConsentServer(t, ctrl, siteconsent.AdminConfig{ConfirmActions: true})
			if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
				t.Fatal(err)
			}
			response := callConsentTool(t, srv, c.tool, c.args)
			isError := strings.Contains(response, `"isError":true`)
			if isError != c.refuse {
				t.Fatalf("%s isError=%v, want %v: %s", c.tool, isError, c.refuse, response)
			}
			if !c.refuse {
				return
			}
			if !strings.Contains(response, "high-risk") || !strings.Contains(response, "non-interactive") {
				t.Fatalf("the refusal must say it is high risk and that nobody could confirm: %s", response)
			}
		})
	}
}

// TestConfirmActionsClassifiesARefFromTheSnapshotBrwReturned proves the label
// path: a click addressed only by ref is classified from the accessible name brw
// itself reported for that ref.
func TestConfirmActionsClassifiesARefFromTheSnapshotBrwReturned(t *testing.T) {
	ctrl := &consentController{
		tabURL:      "https://shop.test/cart",
		findResults: []snapshot.Element{{Ref: "e7", Role: "button", Name: "Place order"}, {Ref: "e8", Role: "button", Name: "Continue shopping"}},
	}
	srv, guard := newConsentServer(t, ctrl, siteconsent.AdminConfig{ConfirmActions: true})
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}

	// Before the find, brw has never seen e7, so there is no label to classify.
	if response := callConsentTool(t, srv, "brw_click", map[string]any{"ref": "e7"}); strings.Contains(response, `"isError":true`) {
		t.Fatalf("an unknown ref must not be refused on a label brw never saw: %s", response)
	}

	if response := callConsentTool(t, srv, "brw_find", map[string]any{"query": "button"}); strings.Contains(response, `"isError":true`) {
		t.Fatalf("find failed: %s", response)
	}
	response := callConsentTool(t, srv, "brw_click", map[string]any{"ref": "e7"})
	if !strings.Contains(response, `"isError":true`) {
		t.Fatalf("a ref whose reported label is a purchase control was not gated: %s", response)
	}
	if !strings.Contains(response, "purchase") {
		t.Fatalf("the refusal does not name the class: %s", response)
	}
	if response := callConsentTool(t, srv, "brw_click", map[string]any{"ref": "e8"}); strings.Contains(response, `"isError":true`) {
		t.Fatalf("an ordinary ref was gated: %s", response)
	}
}

// TestConsentDisabledLeavesToolsUngated is the opt-in guarantee.
func TestConsentDisabledLeavesToolsUngated(t *testing.T) {
	ctrl := &consentController{tabURL: "https://shop.test/cart"}
	srv := New(ctrl)
	if response := callConsentTool(t, srv, "brw_open", map[string]any{"url": "https://anything.test/"}); strings.Contains(response, `"isError":true`) {
		t.Fatalf("a server with no consent store refused a call: %s", response)
	}
	if ctrl.openURL != "https://anything.test/" {
		t.Fatalf("the open did not reach the controller: %q", ctrl.openURL)
	}
}

// TestConsentRulesNameRealTools stops the table drifting from the catalogue.
func TestConsentRulesNameRealTools(t *testing.T) {
	known := map[string]bool{}
	for _, tl := range tools() {
		if name, ok := tl["name"].(string); ok {
			known[name] = true
		}
	}
	for name := range siteconsent.ToolRules {
		if !known[name] {
			t.Errorf("siteconsent.ToolRules names %q, which is not a registered tool", name)
		}
	}
}

// TestEveryDestinationArgumentIsRefused walks the catalogue for tools that take
// a destination argument - whatever that tool calls it - and drives each one at
// an un-granted origin through the real call path.
//
// Asserting BEHAVIOUR rather than table membership is the point. The membership
// version of this test passed for brw_authenticate and brw_cookies while both
// were ungated in their real argument shape: one is addressed by origin, the
// other by domain, and the gate read only url.
func TestEveryDestinationArgumentIsRefused(t *testing.T) {
	// Exempt properties name something other than a destination brw steers to
	// or fetches.
	exempt := map[string]string{
		"brw_pushstate":     "url is a same-document History API change, which loads no document and cannot leave the origin",
		"brw_recipe_search": "origin filters the recipe index; it is not a site brw reaches",
	}
	destinations := map[string]any{
		"url":     "https://ungranted.test/x",
		"origin":  "https://ungranted.test",
		"domain":  "ungranted.test",
		"origins": []any{map[string]any{"origin": "https://ungranted.test", "headers": map[string]any{"X-Test": "1"}}},
	}
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		schema, _ := tl["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if reason := exempt[name]; reason != "" {
			continue
		}
		for field, value := range destinations {
			if _, takes := props[field]; !takes {
				continue
			}
			t.Run(name+"/"+field, func(t *testing.T) {
				ctrl := &consentController{tabURL: "https://start.test/"}
				srv, guard := newConsentServer(t, ctrl, siteconsent.AdminConfig{})
				// The tab brw is on is granted, so the only thing that can
				// refuse is the destination in the argument.
				if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://start.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
					t.Fatal(err)
				}
				response := callConsentTool(t, srv, name, map[string]any{field: value})
				if !strings.Contains(response, "https://ungranted.test") {
					t.Fatalf("%s addressed by %q reached an un-granted origin: %s", name, field, response)
				}
				if !strings.Contains(response, `"isError":true`) {
					t.Fatalf("%s addressed by %q was not refused: %s", name, field, response)
				}
			})
		}
	}
}

// TestEveryToolIsClassifiedForConsent makes the table exhaustive by
// construction. A new tool is either gated or deliberately ungated with the
// reason; one that is neither fails here instead of shipping with no rule.
func TestEveryToolIsClassifiedForConsent(t *testing.T) {
	known := map[string]bool{}
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		known[name] = true
		_, gated := siteconsent.ToolRules[name]
		_, sequence := siteconsent.SequenceTools[name]
		_, ungated := siteconsent.UngatedTools[name]
		if !gated && !sequence && !ungated {
			t.Errorf("tool %q is in neither siteconsent.ToolRules nor siteconsent.UngatedTools; every tool needs a rule or a written reason it needs none", name)
		}
	}
	for name := range siteconsent.UngatedTools {
		if !known[name] {
			t.Errorf("siteconsent.UngatedTools names %q, which is not a registered tool", name)
		}
	}
}
