package snapshot_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// awaitPromise lets a chromedp.Evaluate await an async IIFE's promise.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

// TestWebMCPRuntimeCapturesAndCallsPageTools proves the opt-in WebMCP runtime:
// brw installs the shim at document-start, a cooperating page registers a tool
// via navigator.modelContext, and brw can both list it (PageToolsScript) and
// invoke it (CallPageToolScript) — returning the tool's result.
func TestWebMCPRuntimeCapturesAndCallsPageTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body>
<h1>WebMCP demo</h1>
<script>
  navigator.modelContext.registerTool({
    name: "add_to_cart",
    description: "Add a product to the cart by id",
    inputSchema: { type: "object", properties: { id: { type: "string" } }, required: ["id"] },
    execute: async function(args){ return { added: args.id, cartCount: 1 }; }
  });
</script>
</body></html>`))
	}))
	defer srv.Close()

	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	// Arm the WebMCP runtime BEFORE navigating so navigator.modelContext exists
	// when the page's inline script registers its tool.
	if err := snapshot.RegisterWebMCPOnNewDocument(ctx); err != nil {
		t.Skipf("document-start registration unavailable in this harness: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL),
		chromedp.WaitVisible("h1", chromedp.ByQuery),
		chromedp.Sleep(150*time.Millisecond),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// List page tools.
	var list struct {
		Supported bool `json:"supported"`
		Tools     []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(snapshot.PageToolsScript, &list, awaitPromise)); err != nil {
		t.Fatalf("page tools eval: %v", err)
	}
	if !list.Supported || len(list.Tools) != 1 || list.Tools[0].Name != "add_to_cart" {
		t.Fatalf("expected one registered tool add_to_cart, got %+v", list)
	}

	// Call the page tool and verify its result round-trips.
	var call struct {
		OK     bool `json:"ok"`
		Result struct {
			Added     string `json:"added"`
			CartCount int    `json:"cartCount"`
		} `json:"result"`
		Error string `json:"error"`
	}
	expr := snapshot.CallPageToolScript("add_to_cart", []byte(`{"id":"SKU-42"}`))
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &call, awaitPromise)); err != nil {
		t.Fatalf("call page tool eval: %v", err)
	}
	if !call.OK || call.Result.Added != "SKU-42" || call.Result.CartCount != 1 {
		t.Fatalf("unexpected page tool result: %+v (err=%q)", call, call.Error)
	}
}

// TestWebMCPUnsupportedWhenAbsent confirms a page with no WebMCP runtime reports
// supported:false rather than erroring.
func TestWebMCPUnsupportedWhenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>plain</h1></body></html>`))
	}))
	defer srv.Close()

	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL),
		chromedp.WaitVisible("h1", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	var list struct {
		Supported bool `json:"supported"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(snapshot.PageToolsScript, &list, awaitPromise)); err != nil {
		t.Fatalf("page tools eval: %v", err)
	}
	if list.Supported {
		t.Fatalf("expected supported:false on a page without WebMCP")
	}
}
