package approvalgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/approval"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/siteconsent"
)

type gateController struct {
	browser.Controller
	lists       atomic.Int32
	evaluations atomic.Int32
	state       map[string]any
	evaluateErr error
}

func (c *gateController) ListTabs(context.Context) ([]browser.Tab, error) {
	c.lists.Add(1)
	return []browser.Tab{{ID: "tab-1", URL: "https://example.test", Active: true}}, nil
}

func (c *gateController) Evaluate(ctx context.Context, expression string) (any, error) {
	c.evaluations.Add(1)
	if browser.TabIDFromContext(ctx) == "" {
		return nil, errors.New("capture was not pinned to a tab")
	}
	if expression == "" {
		return nil, errors.New("empty capture expression")
	}
	return c.state, c.evaluateErr
}

func controller() *gateController {
	return &gateController{state: map[string]any{
		"url": "https://example.test/form", "complete": true,
		"preview": map[string]any{"title": "Approval fixture", "text": "Ready to submit"},
		"text":    "Ready to submit", "time_origin": float64(1700000000000),
	}}
}

func newGate(t testing.TB, mode string) (*Gate, *gateController, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "approvals.json")
	s, err := approval.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := controller()
	g, err := New(c, s, mode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return g, c, path
}

func pending(t testing.TB, g *Gate, ctx context.Context, raw json.RawMessage, session string) *RequiredError {
	t.Helper()
	_, _, err := g.Check(ctx, "brw_click", raw, session, nil, false)
	var required *RequiredError
	if !errors.As(err, &required) || required.Status != approval.Pending || required.RequestID == "" {
		t.Fatalf("expected pending request, got %v", err)
	}
	return required
}

func approved(t testing.TB, g *Gate, id string) {
	t.Helper()
	if _, err := g.Store.Decide(id, approval.Approved, "operator", "approved fixture"); err != nil {
		t.Fatal(err)
	}
}

func retryArgs(t testing.TB, raw json.RawMessage, id string) json.RawMessage {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	args["approval_id"] = id
	result, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func code(t testing.TB, err error, want string) {
	t.Helper()
	var failure *approval.Error
	if !errors.As(err, &failure) || failure.Code != want {
		t.Fatalf("got %v, want approval code %s", err, want)
	}
}

func TestNilAndOrdinaryReadAvoidController(t *testing.T) {
	ctx := context.Background()
	raw := json.RawMessage(`{"selector":"main"}`)
	var absent *Gate
	got, clean, err := absent.Check(ctx, "brw_click", raw, "session", nil, true)
	if err != nil || got != ctx || !bytes.Equal(clean, raw) {
		t.Fatalf("nil gate changed request: %v", err)
	}
	for _, mode := range []string{"all", "risky"} {
		t.Run(mode, func(t *testing.T) {
			g, c, _ := newGate(t, mode)
			for _, tool := range []string{"brw_read", "brw_snapshot", "brw_tabs", "brw_screenshot"} {
				got, clean, err := g.Check(ctx, tool, raw, "session", nil, false)
				if err != nil || got != ctx || !bytes.Equal(clean, raw) {
					t.Fatalf("read %s changed request: %v", tool, err)
				}
			}
			if c.lists.Load() != 0 || c.evaluations.Load() != 0 {
				t.Fatal("ordinary reads reached browser controller")
			}
			if len(g.Store.List()) != 0 {
				t.Fatal("ordinary reads created approval records")
			}
		})
	}
}

func TestRiskyKnownBenignActionUsesExistingLabel(t *testing.T) {
	g, c, _ := newGate(t, "risky")
	var labels int
	label := siteconsent.LabelFunc(func(ref string) string {
		labels++
		if ref != "button-ref" {
			t.Fatalf("unexpected ref %s", ref)
		}
		return "Expand details"
	})
	raw := json.RawMessage(`{"ref":"button-ref"}`)
	_, clean, err := g.Check(context.Background(), "brw_click", raw, "session", label, false)
	if err != nil || !bytes.Equal(clean, raw) {
		t.Fatalf("benign action was gated: %v", err)
	}
	if labels == 0 {
		t.Fatal("benign action was not classified using its label")
	}
	if c.lists.Load() != 0 || c.evaluations.Load() != 0 {
		t.Fatal("benign classification reached browser controller")
	}
}

func TestPendingDeduplicatesWithoutRecapturing(t *testing.T) {
	g, c, _ := newGate(t, "all")
	ctx := browser.WithTabID(context.Background(), "tab-1")
	raw := json.RawMessage(`{"query":"#submit"}`)
	first := pending(t, g, ctx, raw, "session-stable")
	second := pending(t, g, ctx, raw, "session-stable")
	if first.RequestID != second.RequestID || len(g.Store.List()) != 1 {
		t.Fatal("pending request was not deduplicated")
	}
	if c.lists.Load() != 0 || c.evaluations.Load() != 1 {
		t.Fatalf("duplicate touched browser: lists=%d evaluations=%d", c.lists.Load(), c.evaluations.Load())
	}
}

func TestExactRetryConsumesOnceAndPinsTab(t *testing.T) {
	g, c, _ := newGate(t, "all")
	raw := json.RawMessage(`{"query":"#submit","button":"left"}`)
	first := pending(t, g, context.Background(), raw, "session-stable")
	req, ok := g.Store.Get(first.RequestID)
	if !ok || req.TabID != "tab-1" || req.SessionID != "session-stable" {
		t.Fatalf("request not bound to tab/session: %+v", req)
	}
	approved(t, g, first.RequestID)
	retry := retryArgs(t, raw, first.RequestID)
	ctx, clean, err := g.Check(browser.WithTabID(context.Background(), "tab-1"), "brw_click", retry, "session-stable", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if browser.TabIDFromContext(ctx) != "tab-1" {
		t.Fatal("approved action was not pinned")
	}
	if !approval.IsExecution(ctx) {
		t.Fatal("consumed approval did not mark local execution authority")
	}
	var args map[string]any
	if err := json.Unmarshal(clean, &args); err != nil {
		t.Fatal(err)
	}
	if _, exists := args["approval_id"]; exists {
		t.Fatal("approval id leaked into execution arguments")
	}
	if args["query"] != "#submit" || args["button"] != "left" {
		t.Fatalf("execution arguments changed: %s", clean)
	}
	req, _ = g.Store.Get(first.RequestID)
	if req.Status != approval.Consumed {
		t.Fatalf("approval not spent before dispatch: %+v", req)
	}
	_, _, err = g.Check(ctx, "brw_click", retry, "session-stable", nil, false)
	var refusal *RequiredError
	if !errors.As(err, &refusal) || refusal.Status != approval.Consumed {
		t.Fatalf("second retry authorized: %v", err)
	}
	if c.evaluations.Load() != 2 {
		t.Fatalf("consumed retry recaptured state: %d", c.evaluations.Load())
	}
}

func TestConcurrentExactRetriesAuthorizeOnlyOneDispatch(t *testing.T) {
	g, c, _ := newGate(t, "all")
	ctx := browser.WithTabID(context.Background(), "tab-1")
	raw := json.RawMessage(`{"query":"#submit"}`)
	first := pending(t, g, ctx, raw, "session-stable")
	approved(t, g, first.RequestID)
	retry := retryArgs(t, raw, first.RequestID)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := g.Check(ctx, "brw_click", retry, "session-stable", nil, false); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("authorized %d dispatches", successes.Load())
	}
	if c.lists.Load() != 0 {
		t.Fatal("explicit retry changed tabs")
	}
}

func TestApprovalRejectsChangedBindingsBeforeEvidence(t *testing.T) {
	for _, changed := range []string{"arguments", "tab", "session", "tool"} {
		t.Run(changed, func(t *testing.T) {
			g, c, _ := newGate(t, "all")
			ctx := browser.WithTabID(context.Background(), "tab-1")
			raw := json.RawMessage(`{"query":"#submit"}`)
			first := pending(t, g, ctx, raw, "session-stable")
			approved(t, g, first.RequestID)
			tool, session := "brw_click", "session-stable"
			switch changed {
			case "arguments":
				raw = json.RawMessage(`{"query":"#different"}`)
			case "tab":
				ctx = browser.WithTabID(context.Background(), "tab-2")
			case "session":
				session = "session-other"
			case "tool":
				tool = "brw_click_text"
			}
			_, _, err := g.Check(ctx, tool, retryArgs(t, raw, first.RequestID), session, nil, false)
			code(t, err, "binding_mismatch")
			if c.evaluations.Load() != 1 {
				t.Fatal("binding mismatch captured browser state")
			}
			req, _ := g.Store.Get(first.RequestID)
			if req.Status != approval.Approved {
				t.Fatal("binding mismatch consumed unrelated approval")
			}
		})
	}
}

func TestChangedEvidenceInvalidatesApproval(t *testing.T) {
	for _, changed := range []string{"text", "time_origin", "url"} {
		t.Run(changed, func(t *testing.T) {
			g, c, _ := newGate(t, "all")
			ctx := browser.WithTabID(context.Background(), "tab-1")
			raw := json.RawMessage(`{"query":"#submit"}`)
			first := pending(t, g, ctx, raw, "session-stable")
			approved(t, g, first.RequestID)
			switch changed {
			case "text":
				c.state["text"] = "Changed payment amount"
			case "time_origin":
				c.state["time_origin"] = float64(1700000001000)
			case "url":
				c.state["url"] = "https://example.test/other"
			}
			_, _, err := g.Check(ctx, "brw_click", retryArgs(t, raw, first.RequestID), "session-stable", nil, false)
			code(t, err, approval.Stale)
			req, _ := g.Store.Get(first.RequestID)
			if req.Status != approval.Stale {
				t.Fatal("changed evidence left reusable approval")
			}
		})
	}
}

func TestDeniedExpiredAndConsumedDoNotRecaptureOrAuthorize(t *testing.T) {
	for _, status := range []string{approval.Denied, approval.Expired, approval.Consumed} {
		t.Run(status, func(t *testing.T) {
			g, c, path := newGate(t, "all")
			ctx := browser.WithTabID(context.Background(), "tab-1")
			raw := json.RawMessage(`{"query":"#submit"}`)
			first := pending(t, g, ctx, raw, "session-stable")
			req, _ := g.Store.Get(first.RequestID)
			switch status {
			case approval.Denied:
				if _, err := g.Store.Decide(req.ID, approval.Denied, "operator", "no"); err != nil {
					t.Fatal(err)
				}
			case approval.Consumed:
				approved(t, g, req.ID)
				if _, err := g.Store.Consume(req.ID, req.Fingerprint, req.StateDigest); err != nil {
					t.Fatal(err)
				}
			case approval.Expired:
				if err := g.Store.Close(); err != nil {
					t.Fatal(err)
				}
				req.Status = approval.Approved
				req.CreatedAt = time.Now().Add(-2 * time.Minute)
				req.ExpiresAt = time.Now().Add(-time.Minute)
				req.DecidedAt = req.CreatedAt.Add(10 * time.Second)
				data, err := json.Marshal(map[string]any{"version": 1, "requests": []approval.Request{req}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				reopened, err := approval.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { reopened.Close() })
				g.Store = reopened
			}
			_, _, err := g.Check(ctx, "brw_click", retryArgs(t, raw, first.RequestID), "session-stable", nil, false)
			var refused *RequiredError
			if !errors.As(err, &refused) || refused.Status != status {
				t.Fatalf("terminal retry authorized: %v", err)
			}
			if c.evaluations.Load() != 1 {
				t.Fatal("terminal retry captured evidence")
			}
			if status != approval.Expired {
				_, _, err = g.Check(ctx, "brw_click", raw, "session-stable", nil, false)
				if !errors.As(err, &refused) || refused.Status != status || refused.RequestID != first.RequestID {
					t.Fatalf("terminal call requeued: %v", err)
				}
				if c.evaluations.Load() != 1 {
					t.Fatal("terminal duplicate captured evidence")
				}
			}
		})
	}
}

func TestSequencesFailBeforeBrowserEvidence(t *testing.T) {
	for _, tool := range []string{"brw_batch", "brw_plan", "brw_recipe_run"} {
		t.Run(tool, func(t *testing.T) {
			g, c, _ := newGate(t, "all")
			raw := json.RawMessage(`{"steps":[{"action":"click","query":"#submit"}],"name":"fixture"}`)
			_, _, err := g.Check(context.Background(), tool, raw, "session-stable", nil, false)
			if err == nil || !strings.Contains(err.Error(), "approval_split_required") {
				t.Fatalf("sequence was not refused: %v", err)
			}
			if c.evaluations.Load() != 0 || c.lists.Load() != 0 {
				t.Fatal("sequence touched browser before refusal")
			}
			if len(g.Store.List()) != 0 {
				t.Fatal("unsafe whole sequence created approval")
			}
		})
	}
}

func TestErrorAndStatusDoNotRevealMessageContent(t *testing.T) {
	g, c, _ := newGate(t, "all")
	secret := "private-message-for-alice-67248"
	raw := json.RawMessage(`{"query":"#send","text":"` + secret + `","password":"hidden-password"}`)
	first := pending(t, g, browser.WithTabID(context.Background(), "tab-1"), raw, "session-stable")
	details := ErrorDetails(first)
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.Error(), secret) || bytes.Contains(encoded, []byte(secret)) || bytes.Contains(encoded, []byte("hidden-password")) {
		t.Fatal("approval refusal leaked private message content")
	}
	status, err := g.Status(first.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(secret)) || bytes.Contains(encoded, []byte("#send")) || bytes.Contains(encoded, []byte("session-stable")) {
		t.Fatal("public status exposed action content")
	}
	if c.evaluations.Load() != 1 {
		t.Fatal("status lookup touched browser")
	}
	c.evaluateErr = errors.New("controller failed with " + secret)
	other := json.RawMessage(`{"query":"#other"}`)
	_, _, err = g.Check(browser.WithTabID(context.Background(), "tab-1"), "brw_click", other, "session-stable", nil, false)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("evidence refusal leaked controller error: %v", err)
	}
}

func TestOperatorURLsAreDeniedBeforeBrowserAccess(t *testing.T) {
	g, c, _ := newGate(t, "all")
	g.SetOperatorOrigin("http://127.0.0.1:9223/")
	for _, url := range []string{"http://127.0.0.1:9223/approvals", "http://localhost:9223/api/approvals", "http://[::1]:9223/approvals", "http://0.0.0.0:9223/approvals", "http://host.localhost:9223/approvals"} {
		raw, _ := json.Marshal(map[string]string{"url": url})
		if _, _, err := g.Check(context.Background(), "brw_navigate_to", raw, "session-stable", nil, false); err == nil {
			t.Fatalf("operator URL allowed: %s", url)
		}
	}
	if c.evaluations.Load() != 0 || c.lists.Load() != 0 {
		t.Fatal("operator URL denial touched browser")
	}
	c.state["url"] = "http://127.0.0.1:9223/approvals"
	_, _, err := g.Check(browser.WithTabID(context.Background(), "tab-1"), "brw_click", json.RawMessage(`{"query":"#approve"}`), "session-stable", nil, false)
	if err == nil {
		t.Fatal("operator page action allowed")
	}
	if len(g.Store.List()) != 0 {
		t.Fatal("operator page created approval")
	}
}

func BenchmarkApprovalGate(b *testing.B) {
	for _, kind := range []string{"nil", "read", "risky_known_benign"} {
		b.Run(kind, func(b *testing.B) {
			var g *Gate
			var c *gateController
			tool := "brw_read"
			raw := json.RawMessage(`{"selector":"main"}`)
			if kind != "nil" {
				g, c, _ = newGate(b, "risky")
			}
			if kind == "risky_known_benign" {
				tool = "brw_click"
				raw = json.RawMessage(`{"ref":"button-ref"}`)
			}
			label := siteconsent.LabelFunc(func(string) string { return "Expand details" })
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := g.Check(ctx, tool, raw, "session-stable", label, false); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if c != nil && (c.lists.Load() != 0 || c.evaluations.Load() != 0) {
				b.Fatal("ordinary benchmark reached browser")
			}
		})
	}
}

func TestRequiredBenignActionStillNeedsOperator(t *testing.T) {
	g, c, _ := newGate(t, "risky")
	raw := json.RawMessage(`{"query":"Expand details"}`)
	_, _, err := g.Check(browser.WithTabID(context.Background(), "tab-1"), "brw_click", raw, "session-stable", nil, true)
	var required *RequiredError
	if !errors.As(err, &required) || required.Status != approval.Pending {
		t.Fatalf("required consent classification bypassed approval: %v", err)
	}
	if c.evaluations.Load() != 1 {
		t.Fatal("required action did not capture evidence")
	}
}

func TestRevokedConsentBlocksApprovedRetryWithoutConsuming(t *testing.T) {
	g, _, _ := newGate(t, "all")
	dir := t.TempDir()
	store, err := siteconsent.OpenStore(filepath.Join(dir, "grants.json"), filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	guard, err := siteconsent.NewGuard(store, siteconsent.AdminConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://example.test", Scope: siteconsent.ScopeAct, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	g.SetConsentGuard(guard)
	ctx := browser.WithTabID(context.Background(), "tab-1")
	raw := json.RawMessage(`{"query":"#submit"}`)
	first := pending(t, g, ctx, raw, "session-stable")
	approved(t, g, first.RequestID)
	if count, err := guard.Revoke("https://example.test", siteconsent.ScopeAct, "operator"); err != nil || count != 1 {
		t.Fatalf("revoke: %d, %v", count, err)
	}
	got, _, err := g.Check(ctx, "brw_click", retryArgs(t, raw, first.RequestID), "session-stable", nil, false)
	if err == nil || approval.IsExecution(got) {
		t.Fatal("revoked consent granted execution authority")
	}
	req, _ := g.Store.Get(first.RequestID)
	if req.Status != approval.Approved {
		t.Fatal("revoked consent consumed approval despite refusing dispatch")
	}
}

func TestIncompleteEvidenceRefusesRetry(t *testing.T) {
	g, c, _ := newGate(t, "all")
	ctx := browser.WithTabID(context.Background(), "tab-1")
	raw := json.RawMessage(`{"query":"#submit"}`)
	first := pending(t, g, ctx, raw, "session-stable")
	approved(t, g, first.RequestID)
	c.state["complete"] = false
	got, _, err := g.Check(ctx, "brw_click", retryArgs(t, raw, first.RequestID), "session-stable", nil, false)
	if err == nil || approval.IsExecution(got) {
		t.Fatal("incomplete evidence authorized execution")
	}
	req, _ := g.Store.Get(first.RequestID)
	if req.Status != approval.Approved {
		t.Fatal("incomplete evidence spent the approval")
	}
}

func TestRiskyDestructiveAndAccessLabelsRequireApproval(t *testing.T) {
	for _, label := range []string{"Delete account", "Grant access", "Transfer ownership", "Permissions"} {
		t.Run(label, func(t *testing.T) {
			g, _, _ := newGate(t, "risky")
			needed, err := g.needs("brw_click", []byte(`{"ref":"e1"}`), func(string) string { return label })
			if err != nil || !needed {
				t.Fatalf("label %q did not require approval: %v %v", label, needed, err)
			}
		})
	}
}

func TestRiskyBenignBatchRetainsFastPath(t *testing.T) {
	g, c, _ := newGate(t, "risky")
	raw := json.RawMessage(`{"steps":[{"action":"fill","ref":"search","text":"example"},{"action":"assert_value","ref":"search","value":"example"},{"action":"click","ref":"expand"}]}`)
	label := func(ref string) string {
		if ref == "search" {
			return "Search"
		}
		return "Expand details"
	}
	if _, _, err := g.Check(context.Background(), "brw_batch", raw, "session", label, false); err != nil {
		t.Fatal(err)
	}
	if c.lists.Load() != 0 || c.evaluations.Load() != 0 || len(g.Store.List()) != 0 {
		t.Fatal("benign batch used approval slow path")
	}
	protected := json.RawMessage(`{"steps":[{"action":"click_text","text":"Publish"}]}`)
	if _, _, err := g.Check(context.Background(), "brw_batch", protected, "session", label, false); err == nil || !strings.Contains(err.Error(), "approval_split_required") {
		t.Fatalf("protected batch was not split before dispatch: %v", err)
	}
}
