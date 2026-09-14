package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
	// tabURLAfterStep moves the tab once a sequence step has run, which is what
	// a click on a cross-site link does to the steps that follow it.
	tabURLAfterStep map[int]string
	ranSteps        int
}

// ExecuteBatch runs the steps the way the real runners do: ask the installed
// per-step consent gate BEFORE each step, and stop on a refusal. Without this
// the fake would pass a batch the real runner refuses, and what this file exists
// to test - that the server installs the gate at all - would be untested.
func (c *consentController) ExecuteBatch(ctx context.Context, steps []browser.BatchStep) (browser.BatchResult, error) {
	result := browser.BatchResult{OK: true, TabID: "tab1"}
	for index, step := range steps {
		if err := browser.GateSequenceStep(ctx, index, "tab1", step.ConsentProbe()); err != nil {
			result.OK = false
			result.Error = err.Error()
			return result, nil
		}
		c.ranSteps++
		if moved, ok := c.tabURLAfterStep[index+1]; ok {
			c.tabURL = moved
		}
	}
	result.StepsCompleted = len(steps)
	return result, nil
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

// destinationArgumentNames are the argument names a tool could name a network
// destination in. The list is deliberately wider than the gate's own
// DestinationField set: the point of reading it off the published schemas is to
// catch a tool whose destination argument the gate never thought of, which is
// exactly how brw_authenticate shipped gated on "url" while it is addressed by
// "origin".
var destinationArgumentNames = []string{
	"url", "urls", "origin", "origins", "domain", "domains", "host", "hostname",
	"endpoint", "target", "uri", "href", "address", "site", "link", "server",
	"base_url", "src", "source", "path", "pattern",
}

// notADestination names the tool/argument pairs whose name matches but whose
// value is not somewhere brw goes, with the reason. An entry here is a claim a
// reviewer can check against the tool's own schema text.
var notADestination = map[string]string{
	"brw_pushstate/url":            "a same-document History API change, which loads no document and cannot leave the origin",
	"brw_recipe_search/origin":     "filters the recipe index; it is not a site brw reaches",
	"brw_frame/target":             "names a frame already loaded inside the page, which the page's own read grant covers",
	"brw_get/target":               "names an element or a page property, not a destination",
	"brw_cookies/path":             "a cookie path inside a site, not a site",
	"brw_upload_file/path":         "a file on THIS machine, which the daemon reads from disk and never fetches",
	"brw_set_download_path/path":   "a directory on THIS machine",
	"brw_console/pattern":          "filters console lines brw already captured",
	"brw_network_requests/pattern": "filters requests the page already made; it names no request brw issues",
	"brw_network_capture/pattern":  "selects which of the page's own requests to record",
	"brw_route/pattern":            "selects which of the page's own requests a rule applies to",
}

// destinationValue is the un-granted value to drive one argument with, chosen
// from the argument's declared type and name so the tool's own decoder accepts
// it.
func destinationValue(field string, schema map[string]any) any {
	if kind, _ := schema["type"].(string); kind == "array" {
		return []any{map[string]any{"origin": "https://ungranted.test", "headers": map[string]any{"X-Test": "1"}}}
	}
	switch field {
	case "domain", "domains", "host", "hostname":
		return "ungranted.test"
	}
	return "https://ungranted.test/x"
}

// TestEveryDestinationArgumentIsRefused walks the catalogue for tools that take
// a destination argument - whatever that tool calls it - and drives each one at
// an un-granted origin through the real call path.
//
// The argument NAMES come from the published schemas rather than from a list
// written here, so a tool addressed by "target" or "endpoint" is picked up the
// day it ships instead of the day somebody remembers to add the name. Asserting
// BEHAVIOUR rather than table membership is the other half: the membership
// version of this test passed for brw_authenticate and brw_cookies while both
// were ungated in their real argument shape.
func TestEveryDestinationArgumentIsRefused(t *testing.T) {
	covered := 0
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		schema, _ := tl["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, field := range destinationArgumentNames {
			declared, takes := props[field]
			if !takes {
				continue
			}
			if reason := notADestination[name+"/"+field]; reason != "" {
				continue
			}
			propSchema, _ := declared.(map[string]any)
			covered++
			t.Run(name+"/"+field, func(t *testing.T) {
				ctrl := &consentController{tabURL: "https://start.test/"}
				srv, guard := newConsentServer(t, ctrl, siteconsent.AdminConfig{})
				// The tab brw is on is granted, so the only thing that can
				// refuse is the destination in the argument.
				if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://start.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
					t.Fatal(err)
				}
				response := callConsentTool(t, srv, name, map[string]any{field: destinationValue(field, propSchema)})
				if !strings.Contains(response, "ungranted.test") {
					t.Fatalf("%s addressed by %q reached an un-granted origin: %s", name, field, response)
				}
				if !strings.Contains(response, `"isError":true`) {
					t.Fatalf("%s addressed by %q was not refused: %s", name, field, response)
				}
			})
		}
	}
	if covered == 0 {
		t.Fatal("no tool in the catalogue declares a destination argument; the schemas this test reads have moved")
	}
	// An exemption for a tool or argument that no longer exists is an argument
	// nobody is making any more, and it would silently cover a future one.
	known := map[string]bool{}
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		schema, _ := tl["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for field := range props {
			known[name+"/"+field] = true
		}
	}
	for pair := range notADestination {
		if !known[pair] {
			t.Errorf("notADestination exempts %q, which no tool declares", pair)
		}
	}
}

// TestUngatedToolsReachNoSite is the behavioural half of the ungated table.
//
// A written reason is something a reviewer argues with; this is the part a test
// can settle. Every tool brw declares ungated is checked against what its own
// handler does: if it calls a controller method that a GATED tool calls - the
// reads, the actions, the navigations - then it reaches a site, and the reason
// it carries is wrong whatever it says. Both halves of the comparison are read
// out of the source, so neither is a list that can be forgotten.
func TestUngatedToolsReachNoSite(t *testing.T) {
	bodies := toolCaseBodies(t)
	controller := controllerMethodNames(t)
	reaches := map[string]string{}
	for tool := range siteconsent.ToolRules {
		for method := range calledMethods(bodies[tool]) {
			if controller[method] {
				reaches[method] = tool
			}
		}
	}
	if len(reaches) == 0 {
		t.Fatal("no gated tool calls a controller method; the dispatch switch this test reads has moved")
	}
	for tool, reason := range siteconsent.UngatedTools {
		body, dispatched := bodies[tool]
		if !dispatched {
			t.Errorf("%s is declared ungated but callTool has no case for it", tool)
			continue
		}
		for method := range calledMethods(body) {
			if gated, isSiteReaching := reaches[method]; isSiteReaching {
				t.Errorf("%s is declared ungated (%q) but its handler calls %s, which is how %s reaches a site; either it needs a rule or that method does not reach a site",
					tool, reason, method, gated)
			}
		}
	}
}

// toolCaseBodies returns the body of each tool's case in callTool's dispatch
// switch, keyed by tool name.
func toolCaseBodies(t *testing.T) map[string][]ast.Stmt {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var dispatch *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "callTool" {
			dispatch = fn
			break
		}
	}
	if dispatch == nil {
		t.Fatal("server.go declares no callTool; the dispatch this test reads has moved")
	}
	out := map[string][]ast.Stmt{}
	ast.Inspect(dispatch, func(node ast.Node) bool {
		stmt, ok := node.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		for _, item := range stmt.Body.List {
			clause, ok := item.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range clause.List {
				literal, ok := expr.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				if name, err := strconv.Unquote(literal.Value); err == nil {
					out[name] = clause.Body
				}
			}
		}
		return true
	})
	return out
}

// controllerMethodNames reads the browser.Controller interface, which is the
// whole of what a tool handler can ask the browser to do.
func controllerMethodNames(t *testing.T) map[string]bool {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), "../browser/controller.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	ast.Inspect(parsed, func(node ast.Node) bool {
		spec, ok := node.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "Controller" {
			return true
		}
		iface, ok := spec.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		for _, method := range iface.Methods.List {
			for _, name := range method.Names {
				out[name.Name] = true
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatal("browser.Controller declares no methods; the interface this test reads has moved")
	}
	return out
}

// calledMethods returns every method name called through a selector anywhere in
// a statement list.
func calledMethods(body []ast.Stmt) map[string]bool {
	out := map[string]bool{}
	for _, stmt := range body {
		ast.Inspect(stmt, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
				out[selector.Sel.Name] = true
			}
			return true
		})
	}
	return out
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

// TestReadURLIsGatedOnEveryRedirectHop drives the no-browser read against real
// servers: the URL the call names is granted, and it answers with a 302 to one
// that is not.
//
// A grant is for an origin, not for a request. Gating only the first URL made a
// read grant on any site a read of every site it chose to point at, which is
// exactly the redirected document the read scope says it covers.
func TestReadURLIsGatedOnEveryRedirectHop(t *testing.T) {
	const secretBody = "<html><body><p>internal quarterly statement</p></body></html>"
	var secretHits int64
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&secretHits, 1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, secretBody)
	}))
	defer secret.Close()
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, secret.URL+"/statement", http.StatusFound)
	}))
	defer entry.Close()

	ctrl := &consentController{tabURL: "https://start.test/"}
	srv, guard := newConsentServer(t, ctrl, siteconsent.AdminConfig{})
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: entry.URL, Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}

	response := callConsentTool(t, srv, "brw_read_url", map[string]any{"url": entry.URL + "/"})
	if !strings.Contains(response, `"isError":true`) {
		t.Fatalf("a redirect carried the read to an un-granted origin: %s", response)
	}
	if !strings.Contains(response, secret.URL) {
		t.Fatalf("the refusal does not name the origin the redirect reached: %s", response)
	}
	if strings.Contains(response, "quarterly statement") {
		t.Fatalf("the un-granted page's content came back: %s", response)
	}
	if got := atomic.LoadInt64(&secretHits); got != 0 {
		t.Fatalf("the un-granted origin was fetched %d times", got)
	}

	// Granting the destination lets the same chain through, so this is a gate
	// and not a refusal of redirects.
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: secret.URL, Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	response = callConsentTool(t, srv, "brw_read_url", map[string]any{"url": entry.URL + "/"})
	if strings.Contains(response, `"isError":true`) {
		t.Fatalf("the granted chain was still refused: %s", response)
	}
	if !strings.Contains(response, "quarterly statement") {
		t.Fatalf("the granted read returned no content: %s", response)
	}
}

// TestASequenceIsRegatedInsideTheRunner proves the MCP surface installs the
// per-step gate and that it survives the hop into the controller.
//
// The gate itself is tested in internal/siteconsent and the runners' use of it
// in internal/browser. What can only be tested here is the wiring: a server that
// gates the call and then dispatches without installing the step gate leaves
// every step after the first navigation decided by the arguments alone.
func TestASequenceIsRegatedInsideTheRunner(t *testing.T) {
	ctrl := &consentController{tabURL: "https://start.test/"}
	// The runner lands somewhere else after the first step, which is what a
	// click on a cross-site link does.
	ctrl.tabURLAfterStep = map[int]string{1: "https://elsewhere.test/inbox"}
	srv, guard := newConsentServer(t, ctrl, siteconsent.AdminConfig{})
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://start.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}

	steps := []any{
		map[string]any{"action": "click", "ref": "e1"},
		map[string]any{"action": "read"},
	}
	response := callConsentTool(t, srv, "brw_batch", map[string]any{"steps": steps})
	if !strings.Contains(response, "https://elsewhere.test") {
		t.Fatalf("a batch read an origin the click navigated to, with no grant there: %s", response)
	}
	if ctrl.ranSteps != 1 {
		t.Fatalf("the runner ran %d steps; the read must be refused before it runs", ctrl.ranSteps)
	}

	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://elsewhere.test", Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	// The failed batch left the tab where the click took it; a fresh call starts
	// from the page the agent is on.
	ctrl.tabURL = "https://start.test/"
	ctrl.ranSteps = 0
	response = callConsentTool(t, srv, "brw_batch", map[string]any{"steps": steps})
	if strings.Contains(response, `"isError":true`) {
		t.Fatalf("the granted batch was refused: %s", response)
	}
	if ctrl.ranSteps != 2 {
		t.Fatalf("the granted batch ran %d steps, want 2", ctrl.ranSteps)
	}
}
