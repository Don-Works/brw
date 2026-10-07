package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/approval"
	"github.com/Don-Works/brw/internal/approvalgate"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/snapshot"
)

type approvalIntegrationBrowser struct {
	browser.Controller
	mu     sync.Mutex
	state  string
	clicks int
	url    string
}

func (c *approvalIntegrationBrowser) ListTabs(context.Context) ([]browser.Tab, error) {
	pageURL := c.url
	if pageURL == "" {
		pageURL = "https://approval.test/cart"
	}
	return []browser.Tab{{ID: "approval-tab", URL: pageURL, Active: true}}, nil
}

func (c *approvalIntegrationBrowser) Read(context.Context) (readability.PageRead, error) {
	return readability.PageRead{Main: "private operator request"}, nil
}

func (c *approvalIntegrationBrowser) ClickText(context.Context, snapshot.ClickTextOptions) (browser.ActionResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clicks++
	return browser.ActionResult{OK: true}, nil
}

func (c *approvalIntegrationBrowser) Evaluate(ctx context.Context, expression string) (any, error) {
	if browser.TabIDFromContext(ctx) != "approval-tab" || !strings.Contains(expression, "time_origin:performance.timeOrigin") {
		return nil, fmt.Errorf("unexpected evidence evaluation on %q", browser.TabIDFromContext(ctx))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"url": "https://approval.test/cart", "complete": true, "controls": []any{map[string]any{"value": c.state}}, "preview": map[string]any{"title": "Checkout"}}, nil
}

func (c *approvalIntegrationBrowser) Click(ctx context.Context, ref string) (browser.ActionResult, error) {
	if browser.TabIDFromContext(ctx) != "approval-tab" || ref != "e1" || !approval.IsExecution(ctx) {
		return browser.ActionResult{}, fmt.Errorf("unapproved or incorrectly pinned execution")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clicks++
	return browser.ActionResult{OK: true, Message: "clicked exact approved target"}, nil
}

func (c *approvalIntegrationBrowser) setState(value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = value
}
func (c *approvalIntegrationBrowser) clickCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clicks
}

func approvalIntegrationStore(t *testing.T) *approval.Store {
	t.Helper()
	store, err := approval.Open(filepath.Join(t.TempDir(), "private", "requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func installIntegrationApproval(t *testing.T, srv *Server, manager browser.Controller) *approval.Store {
	t.Helper()
	store := approvalIntegrationStore(t)
	gate, err := approvalgate.New(manager, store, "all")
	if err != nil {
		t.Fatal(err)
	}
	srv.SetApprovalGate(gate)
	srv.sessionID = "approval-integration-session"
	return store
}

func callApprovalIntegration(t *testing.T, srv *Server, name string, args any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, rpcErr := srv.callTool(context.Background(), name, raw)
	if rpcErr != nil {
		t.Fatalf("%s RPC error: %+v", name, rpcErr)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func approvalIntegrationID(t *testing.T, out map[string]any) string {
	t.Helper()
	details, ok := out["structuredContent"].(map[string]any)
	if !ok || out["isError"] != true || details["code"] != "approval_required" {
		t.Fatalf("expected typed approval_required: %+v", out)
	}
	id, ok := details["approval_id"].(string)
	if !ok || id == "" {
		t.Fatalf("missing approval ID: %+v", out)
	}
	return id
}

func resumeIntegrationApproval(t *testing.T, srv *Server, id string, args map[string]any) map[string]any {
	t.Helper()
	return callApprovalIntegration(t, srv, "brw_approval_resume", map[string]any{"approval_id": id, "tool": "brw_click", "arguments": args})
}

func TestApprovalIntegrationMCPExactRetryAndConsumed(t *testing.T) {
	manager := &approvalIntegrationBrowser{state: "original"}
	srv := New(manager)
	store := installIntegrationApproval(t, srv, manager)
	args := map[string]any{"ref": "e1", "tab_id": "approval-tab"}
	id := approvalIntegrationID(t, callApprovalIntegration(t, srv, "brw_click", args))
	if manager.clickCount() != 0 {
		t.Fatal("initial request executed")
	}
	if _, err := store.Decide(id, approval.Approved, "operator", "reviewed"); err != nil {
		t.Fatal(err)
	}
	result := resumeIntegrationApproval(t, srv, id, args)
	if result["isError"] == true || manager.clickCount() != 1 {
		t.Fatalf("exact approved resume did not click once: %+v count=%d", result, manager.clickCount())
	}
	current, _ := store.Get(id)
	if current.Status != approval.Consumed {
		t.Fatalf("approval not consumed: %s", current.Status)
	}
	result = resumeIntegrationApproval(t, srv, id, args)
	details, _ := result["structuredContent"].(map[string]any)
	if result["isError"] != true || details["code"] != "approval_consumed" || manager.clickCount() != 1 {
		t.Fatalf("consumed request replayed: %+v count=%d", result, manager.clickCount())
	}
}

func TestApprovalIntegrationMCPChangedStateInvalidates(t *testing.T) {
	manager := &approvalIntegrationBrowser{state: "original"}
	srv := New(manager)
	store := installIntegrationApproval(t, srv, manager)
	args := map[string]any{"ref": "e1", "tab_id": "approval-tab"}
	id := approvalIntegrationID(t, callApprovalIntegration(t, srv, "brw_click", args))
	if _, err := store.Decide(id, approval.Approved, "operator", ""); err != nil {
		t.Fatal(err)
	}
	manager.setState("changed")
	result := resumeIntegrationApproval(t, srv, id, args)
	if result["isError"] != true || manager.clickCount() != 0 {
		t.Fatalf("changed state executed: %+v", result)
	}
	current, _ := store.Get(id)
	if current.Status != approval.Stale {
		t.Fatalf("changed state status = %s", current.Status)
	}
}

func TestApprovalMCPProtectsOperatorPageWithoutSiteConsent(t *testing.T) {
	manager := &approvalIntegrationBrowser{url: "http://127.0.0.1:17310/approvals"}
	srv := New(manager)
	gate, err := approvalgate.New(manager, approvalIntegrationStore(t), "risky")
	if err != nil {
		t.Fatal(err)
	}
	gate.SetOperatorOrigin("http://127.0.0.1:17310")
	srv.SetApprovalGate(gate)
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"brw_read", map[string]any{"tab_id": "approval-tab"}},
		{"brw_click_text", map[string]any{"tab_id": "approval-tab", "text": "Approve once"}},
	} {
		result := callApprovalIntegration(t, srv, call.tool, call.args)
		encoded, _ := json.Marshal(result)
		if result["isError"] != true || !strings.Contains(string(encoded), "operator approval UI") || manager.clickCount() != 0 || strings.Contains(string(encoded), "private operator request") {
			t.Fatalf("operator page reached by %s: %s", call.tool, encoded)
		}
	}
	ctx := srv.withConsentHooks(browser.WithTabID(context.Background(), "approval-tab"), "brw_batch", nil)
	if err := browser.GateSequenceStep(ctx, 0, "approval-tab", siteconsent.StepProbe{Action: "read"}); err == nil {
		t.Fatal("sequence read the operator page with site consent disabled")
	}
	for _, check := range []func(string) error{srv.CheckFetchDestination, srv.CheckFrameRead} {
		if err := check("http://2130706433:17310/approvals"); err == nil {
			t.Fatal("runtime destination check allowed numeric operator origin")
		}
	}
}

func TestApprovalIntegrationMCPRevokedSiteGrantPreventsWrite(t *testing.T) {
	manager := &approvalIntegrationBrowser{state: "original"}
	srv, guard := newConsentServer(t, manager, siteconsent.AdminConfig{})
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://approval.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	store := installIntegrationApproval(t, srv, manager)
	args := map[string]any{"ref": "e1", "tab_id": "approval-tab"}
	id := approvalIntegrationID(t, callApprovalIntegration(t, srv, "brw_click", args))
	if _, err := store.Decide(id, approval.Approved, "operator", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Revoke("https://approval.test", siteconsent.ScopeAct, "fixture-user"); err != nil {
		t.Fatal(err)
	}
	result := resumeIntegrationApproval(t, srv, id, args)
	if result["isError"] != true || manager.clickCount() != 0 {
		t.Fatalf("revoked site grant still executed: %+v", result)
	}
	current, _ := store.Get(id)
	if current.Status != approval.Approved {
		t.Fatalf("site denial consumed approval: %s", current.Status)
	}
}

func TestApprovalIntegrationMCPStripsApprovalIDBeforeStrictDecode(t *testing.T) {
	manager := &approvalIntegrationBrowser{state: "original"}
	srv := New(manager)
	store := installIntegrationApproval(t, srv, manager)
	args := map[string]any{"ref": "e1", "tab_id": "approval-tab"}
	id := approvalIntegrationID(t, callApprovalIntegration(t, srv, "brw_click", args))
	if _, err := store.Decide(id, approval.Approved, "operator", ""); err != nil {
		t.Fatal(err)
	}
	args["approval_id"] = id
	raw, _ := json.Marshal(args)
	ctx := browser.WithTabID(context.Background(), "approval-tab")
	ctx, clean, err := srv.checkApproval(ctx, "brw_click", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	var strict struct {
		Ref   string `json:"ref"`
		TabID string `json:"tab_id"`
	}
	if err = unmarshalStrictArgs(clean, &strict); err != nil {
		t.Fatalf("approval metadata leaked into strict decoder: %s: %v", clean, err)
	}
	if !approval.IsExecution(ctx) || strict.Ref != "e1" || strict.TabID != "approval-tab" {
		t.Fatal("approved context or arguments lost")
	}
	if manager.clickCount() != 0 {
		t.Fatal("gate preparation executed action")
	}
}

func TestApprovalIntegrationRealChromiumEvidenceAndClick(t *testing.T) {
	chrome, err := cdp.FindChrome("")
	if err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	profile := browsertest.NewProfile(t)
	manager, err := browser.New(ctx, browser.Config{ChromePath: chrome, UserDataDir: profile.Dir(), Headless: true, Timeout: 20 * time.Second})
	if err != nil {
		t.Skipf("headless Chrome did not start: %v", err)
	}
	profile.StopWith(func() { _ = manager.Close() })
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Approval fixture</title></head><body><label for="amount">Amount</label><input id="amount" value="10"><button onclick="window.clicks++">Submit purchase</button><button onclick="window.benignClicks++">Expand details</button><script>window.clicks=0;window.benignClicks=0</script></body></html>`)
	}))
	t.Cleanup(fixture.Close)
	opened, err := manager.Open(ctx, fixture.URL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := browser.WithTabID(ctx, opened.Tab.ID)
	found, err := manager.Find(tabCtx, snapshot.FindOptions{Query: "Submit purchase", Role: "button"})
	if err != nil || len(found.Elements) != 1 {
		t.Fatalf("find real target: %+v %v", found, err)
	}
	args := map[string]any{"ref": found.Elements[0].Ref, "tab_id": opened.Tab.ID}
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed=%v", changed), func(t *testing.T) {
			srv := New(manager)
			store := installIntegrationApproval(t, srv, manager)
			id := approvalIntegrationID(t, callApprovalIntegration(t, srv, "brw_click", args))
			request, _ := store.Get(id)
			if request.StateDigest == "" || !strings.Contains(string(request.Arguments), "Amount") && !strings.Contains(string(request.Arguments), "amount") {
				t.Fatalf("real evidence not captured: %s", request.Arguments)
			}
			if _, err := store.Decide(id, approval.Approved, "operator", ""); err != nil {
				t.Fatal(err)
			}
			if changed {
				if _, err := manager.Evaluate(tabCtx, `document.getElementById('amount').value='999'`); err != nil {
					t.Fatal(err)
				}
			}
			result := resumeIntegrationApproval(t, srv, id, args)
			count, err := manager.Evaluate(tabCtx, "window.clicks")
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(count)
			if string(encoded) != "1" {
				t.Fatalf("real browser click count=%s result=%+v", encoded, result)
			}
			current, _ := store.Get(id)
			if changed {
				if result["isError"] != true || current.Status != approval.Stale {
					t.Fatalf("changed real form was approved: %+v state=%s", result, current.Status)
				}
			} else if result["isError"] == true || current.Status != approval.Consumed {
				t.Fatalf("real click failed: %+v state=%s", result, current.Status)
			}
		})
	}
	srv := New(manager)
	gate, err := approvalgate.New(manager, approvalIntegrationStore(t), "risky")
	if err != nil {
		t.Fatal(err)
	}
	gate.SetOperatorOrigin(fixture.URL)
	srv.SetApprovalGate(gate)
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"brw_read", map[string]any{"tab_id": opened.Tab.ID}},
		{"brw_click_text", map[string]any{"tab_id": opened.Tab.ID, "text": "Expand details"}},
		{"brw_open", map[string]any{"url": strings.Replace(fixture.URL, "127.0.0.1", "2130706433", 1)}},
	} {
		result := callApprovalIntegration(t, srv, call.tool, call.args)
		encoded, _ := json.Marshal(result)
		if result["isError"] != true || !strings.Contains(string(encoded), "operator approval UI") {
			t.Fatalf("real operator page reached by %s: %s", call.tool, encoded)
		}
	}
	value, err := manager.Evaluate(tabCtx, "window.benignClicks")
	if err != nil || fmt.Sprint(value) != "0" {
		t.Fatalf("real operator page was actuated: %v %v", value, err)
	}
}
