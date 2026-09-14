package snapshot_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// chromedpEvaluator is the transport seam the daemon fills with the controller's
// Evaluate. Wiring a real headless tab into it runs the production invocation
// path — the same builders, the same poll loop — against real WebMCP tools.
func chromedpEvaluator(ctx context.Context) snapshot.PageToolEvaluator {
	return func(_ context.Context, expression string) (any, error) {
		var out any
		if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &out, awaitPromise)); err != nil {
			return nil, err
		}
		return out, nil
	}
}

// slowToolPage registers three tools: one that never settles on its own, one
// that settles after a delay, and one that counts how often it was dispatched so
// a test can prove a refusal happened before dispatch rather than inside it.
const slowToolPage = `<!doctype html><html><body>
<h1>WebMCP long jobs</h1>
<script>
  window.__dispatches = 0;
  navigator.modelContext.registerTool({
    name: "export_ledger",
    description: "Export the ledger, which takes as long as it takes",
    inputSchema: { type: "object", properties: { format: { type: "string" } }, required: ["format"] },
    execute: function(args, options){
      window.__dispatches++;
      return new Promise(function(resolve, reject){
        // Never resolves on its own: the only way out is a cancel or a timeout.
        if (options && options.signal) {
          options.signal.addEventListener('abort', function(){ reject(new Error('aborted by the agent')); });
        }
      });
    }
  });
  navigator.modelContext.registerTool({
    name: "throw_now",
    description: "Fail the instant it is called",
    inputSchema: { type: "object", properties: {} },
    execute: function(){
      window.__dispatches++;
      // Synchronous: the invocation is already settled when the start script
      // reaches its return statement.
      throw new Error("ledger is locked");
    }
  });
  navigator.modelContext.registerTool({
    name: "settle_soon",
    description: "Finish after a short delay",
    inputSchema: { type: "object", properties: { ms: { type: "number" } } },
    execute: function(args){
      window.__dispatches++;
      return new Promise(function(resolve){
        setTimeout(function(){ resolve({ finished: true, waited: args.ms }); }, args.ms);
      });
    }
  });
</script>
</body></html>`

func servePage(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return servePages(t, map[string]string{"/": body})
}

// servePages serves several documents from ONE origin, which is what makes an
// iframe same-origin: httptest hands each server its own port, and a different
// port is a different origin.
func servePages(t *testing.T, pages map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// armedWebMCPTab opens a headless tab with the WebMCP runtime armed at
// document-start and navigates it to url.
func armedWebMCPTab(t *testing.T, url string) context.Context {
	t.Helper()
	browserCtx, cancel := newHeadlessChrome(t)
	t.Cleanup(cancel)
	ctx, ctxCancel := context.WithTimeout(browserCtx, 90*time.Second)
	t.Cleanup(ctxCancel)

	if err := snapshot.RegisterWebMCPOnNewDocument(ctx); err != nil {
		t.Skipf("document-start registration unavailable in this harness: %v", err)
	}
	if err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible("h1", chromedp.ByQuery),
		chromedp.Sleep(150*time.Millisecond),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	return ctx
}

func dispatchCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	var count int
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__dispatches || 0`, &count)); err != nil {
		t.Fatalf("read dispatch count: %v", err)
	}
	return count
}

// A page tool slower than the caller's patience must be startable detached,
// pollable by id, and stoppable — the whole point of the invocation registry.
func TestDetachedPageToolIsPolledAndCancelled(t *testing.T) {
	srv := servePage(t, slowToolPage)
	ctx := armedWebMCPTab(t, srv.URL)
	eval := chromedpEvaluator(ctx)

	// A synchronous call cannot finish this tool, and says so without losing it.
	waited, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
		Name:      "export_ledger",
		Arguments: []byte(`{"format":"csv"}`),
		Validate:  true,
		Timeout:   400 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("synchronous invoke: %v", err)
	}
	if !waited.TimedOut || waited.Status != snapshot.PageToolRunning || waited.ID == "" {
		t.Fatalf("a tool slower than the timeout should time out with its id intact, got %+v", waited)
	}

	// Detached: the start returns immediately with an id.
	started, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
		Name:      "export_ledger",
		Arguments: []byte(`{"format":"csv"}`),
		Detach:    true,
		Validate:  true,
	})
	if err != nil {
		t.Fatalf("detached invoke: %v", err)
	}
	if !started.OK || !started.Detached || started.ID == "" || started.Status != snapshot.PageToolRunning {
		t.Fatalf("detached invoke = %+v, want ok running with an id", started)
	}
	if started.ID == waited.ID {
		t.Fatalf("two invocations share id %q", started.ID)
	}

	// Polling reports it still running, without blocking.
	polled, err := snapshot.AwaitPageTool(ctx, eval, started.ID, 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if polled.Status != snapshot.PageToolRunning || polled.TimedOut {
		t.Fatalf("poll = %+v, want a plain running report", polled)
	}

	// Cancelling settles it, and the page tool sees its AbortSignal fire.
	cancelled, err := snapshot.CancelPageTool(ctx, eval, started.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !cancelled.Cancelled || cancelled.Status != snapshot.PageToolCancelled {
		t.Fatalf("cancel = %+v, want status cancelled", cancelled)
	}

	after, err := snapshot.AwaitPageTool(ctx, eval, started.ID, 2*time.Second)
	if err != nil {
		t.Fatalf("poll after cancel: %v", err)
	}
	if after.Status != snapshot.PageToolCancelled || after.OK {
		t.Fatalf("post-cancel poll = %+v, want a settled cancelled invocation", after)
	}

	// Cancelling a settled invocation reports the outcome rather than erroring.
	again, err := snapshot.CancelPageTool(ctx, eval, started.ID)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if again.Cancelled || again.Status != snapshot.PageToolCancelled {
		t.Fatalf("second cancel = %+v, want cancelled:false on an already settled invocation", again)
	}

	// An id the living document never minted is "unknown", not "lost": the page
	// is fine and the id is wrong, which is the opposite diagnosis.
	nonce, _, found := strings.Cut(started.ID, "-")
	if !found {
		t.Fatalf("invocation id %q is not <nonce>-<seq>", started.ID)
	}
	unknown, err := snapshot.AwaitPageTool(ctx, eval, nonce+"-99999", 0)
	if err != nil {
		t.Fatalf("poll an unissued id: %v", err)
	}
	if unknown.Status != snapshot.PageToolUnknown {
		t.Fatalf("unissued id = %+v, want status unknown", unknown)
	}

	// A detached invocation that finishes on its own is collected by id.
	slow, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
		Name:      "settle_soon",
		Arguments: []byte(`{"ms":700}`),
		Detach:    true,
		Validate:  true,
	})
	if err != nil {
		t.Fatalf("detached settle_soon: %v", err)
	}
	collected, err := snapshot.AwaitPageTool(ctx, eval, slow.ID, 10*time.Second)
	if err != nil {
		t.Fatalf("collect settle_soon: %v", err)
	}
	if !collected.OK || collected.Status != snapshot.PageToolDone {
		t.Fatalf("collect = %+v, want a completed invocation", collected)
	}
	var payload struct {
		Finished bool    `json:"finished"`
		Waited   float64 `json:"waited"`
	}
	if err := json.Unmarshal(collected.Result, &payload); err != nil {
		t.Fatalf("decode result %s: %v", collected.Result, err)
	}
	if !payload.Finished || payload.Waited != 700 {
		t.Fatalf("result = %+v, want the tool's own payload", payload)
	}
}

// Oversized arguments must be refused in the daemon, before anything reaches the
// page: the tool is never dispatched and the payload is never truncated.
func TestOversizedPageToolInputIsRefusedBeforeDispatch(t *testing.T) {
	srv := servePage(t, slowToolPage)
	ctx := armedWebMCPTab(t, srv.URL)
	eval := chromedpEvaluator(ctx)

	before := dispatchCount(t, ctx)

	oversized, err := json.Marshal(map[string]string{"format": strings.Repeat("x", snapshot.MaxPageToolInputBytes)})
	if err != nil {
		t.Fatalf("build oversized arguments: %v", err)
	}
	if _, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
		Name:      "export_ledger",
		Arguments: oversized,
		Validate:  true,
		Timeout:   time.Second,
	}); !errors.Is(err, snapshot.ErrPageToolInputTooLarge) {
		t.Fatalf("oversized invoke error = %v, want ErrPageToolInputTooLarge", err)
	}
	if got := dispatchCount(t, ctx); got != before {
		t.Fatalf("page tool was dispatched %d times for a refused payload (was %d)", got, before)
	}

	// The cap is a refusal, not a trim: a payload just under it still runs.
	fits, err := json.Marshal(map[string]string{"format": strings.Repeat("x", 1024)})
	if err != nil {
		t.Fatalf("build in-cap arguments: %v", err)
	}
	accepted, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
		Name:      "export_ledger",
		Arguments: fits,
		Detach:    true,
		Validate:  true,
	})
	if err != nil {
		t.Fatalf("in-cap invoke: %v", err)
	}
	if !accepted.OK {
		t.Fatalf("in-cap invoke = %+v, want it accepted", accepted)
	}
	if got := dispatchCount(t, ctx); got != before+1 {
		t.Fatalf("dispatch count = %d, want %d after one accepted invocation", got, before+1)
	}
}

// Arguments that contradict the tool's declared inputSchema are refused before
// dispatch too, with the tool left untouched.
func TestPageToolInputValidationRefusesBeforeDispatch(t *testing.T) {
	srv := servePage(t, slowToolPage)
	ctx := armedWebMCPTab(t, srv.URL)
	eval := chromedpEvaluator(ctx)

	before := dispatchCount(t, ctx)
	for _, tc := range []struct {
		name string
		args string
		want string
	}{
		{name: "missing required", args: `{}`, want: "requires format"},
		{name: "wrong type", args: `{"format":7}`, want: "format should be string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
				Name:      "export_ledger",
				Arguments: []byte(tc.args),
				Validate:  true,
				Timeout:   time.Second,
			})
			if err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if got.OK || got.Status != snapshot.PageToolRejected || !strings.Contains(got.Error, tc.want) {
				t.Fatalf("invoke = %+v, want it refused naming %q", got, tc.want)
			}
		})
	}
	if got := dispatchCount(t, ctx); got != before {
		t.Fatalf("page tool was dispatched %d times for refused arguments (was %d)", got, before)
	}

	// Turning validation off hands the arguments to the tool as given: the same
	// arguments the schema refused above now reach it, which only the page's own
	// dispatch counter can prove.
	got, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
		Name:      "export_ledger",
		Arguments: []byte(`{}`),
		Detach:    true,
	})
	if err != nil {
		t.Fatalf("unvalidated invoke: %v", err)
	}
	if !got.OK || got.Status != snapshot.PageToolRunning {
		t.Fatalf("unvalidated invoke = %+v, want the page to decide", got)
	}
	if after := dispatchCount(t, ctx); after != before+1 {
		t.Fatalf("dispatch count = %d, want %d: the unvalidated arguments never reached the tool", after, before+1)
	}
}

// A WebMCP tool is registered per document, so an embedded widget's tools live
// in the iframe. Frame targeting must reach them, and must say so plainly when
// the frame is one the browser isolates.
func TestPageToolFrameTargetingReachesAnIframeTool(t *testing.T) {
	// A second server is a second port, which the browser treats as a separate
	// origin: that is the isolated frame this test also needs.
	isolated := servePage(t, `<!doctype html><html><body><h2>isolated</h2></body></html>`)

	widget := `<!doctype html><html><body><h2>widget</h2>
<script>
  navigator.modelContext.registerTool({
    name: "widget_quote",
    description: "Quote a shipment from inside the embedded widget",
    inputSchema: { type: "object", properties: { sku: { type: "string" } }, required: ["sku"] },
    execute: function(args){ return { quoted: args.sku, currency: "GBP" }; }
  });
</script></body></html>`

	host := fmt.Sprintf(`<!doctype html><html><body>
<h1>host</h1>
<script>
  navigator.modelContext.registerTool({
    name: "host_only",
    description: "Registered by the top document",
    inputSchema: { type: "object", properties: {} },
    execute: function(){ return { from: "host" }; }
  });
</script>
<iframe id="widget" src="/widget" style="width:300px;height:200px"></iframe>
<iframe id="isolated" src="%s" style="width:300px;height:200px"></iframe>
</body></html>`, isolated.URL)

	srv := servePages(t, map[string]string{"/": host, "/widget": widget})
	ctx := armedWebMCPTab(t, srv.URL)
	eval := chromedpEvaluator(ctx)
	if err := chromedp.Run(ctx, chromedp.Sleep(300*time.Millisecond)); err != nil {
		t.Fatalf("settle frames: %v", err)
	}

	type listing struct {
		Supported bool   `json:"supported"`
		Frame     string `json:"frame"`
		Error     string `json:"error"`
		Tools     []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	listTools := func(t *testing.T, frame string) listing {
		t.Helper()
		var got listing
		if err := chromedp.Run(ctx, chromedp.Evaluate(snapshot.BuildPageToolsExpression(frame), &got, awaitPromise)); err != nil {
			t.Fatalf("list page tools in frame %q: %v", frame, err)
		}
		return got
	}

	top := listTools(t, "")
	if len(top.Tools) != 1 || top.Tools[0].Name != "host_only" {
		t.Fatalf("top document listing = %+v, want only host_only", top)
	}
	inFrame := listTools(t, "#widget")
	if len(inFrame.Tools) != 1 || inFrame.Tools[0].Name != "widget_quote" || inFrame.Frame != "#widget" {
		t.Fatalf("frame listing = %+v, want only widget_quote", inFrame)
	}

	// The frame's tool is invisible from the top document, which is what makes
	// frame targeting load-bearing rather than a convenience.
	unreachable, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
		Name:      "widget_quote",
		Arguments: []byte(`{"sku":"SKU-9"}`),
		Validate:  true,
		Timeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("invoke without a frame: %v", err)
	}
	if unreachable.OK || unreachable.Status != snapshot.PageToolNotFound || !strings.Contains(unreachable.Error, "page tool not found") {
		t.Fatalf("invoke without a frame = %+v, want not found", unreachable)
	}

	got, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
		Name:      "widget_quote",
		Arguments: []byte(`{"sku":"SKU-9"}`),
		Frame:     "#widget",
		Validate:  true,
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("invoke in frame: %v", err)
	}
	if !got.OK || got.Status != snapshot.PageToolDone || got.Frame != "#widget" {
		t.Fatalf("frame invoke = %+v, want a completed in-frame invocation", got)
	}
	var quote struct {
		Quoted   string `json:"quoted"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(got.Result, &quote); err != nil {
		t.Fatalf("decode frame result %s: %v", got.Result, err)
	}
	if quote.Quoted != "SKU-9" || quote.Currency != "GBP" {
		t.Fatalf("frame result = %+v, want the widget's own answer", quote)
	}

	// A cross-origin frame is named as such, not reported as an empty document.
	isolatedListing := listTools(t, "#isolated")
	if isolatedListing.Supported || !strings.Contains(isolatedListing.Error, "cross-origin") {
		t.Fatalf("cross-origin listing = %+v, want a named cross-origin refusal", isolatedListing)
	}
	missing, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
		Name:      "widget_quote",
		Arguments: []byte(`{"sku":"SKU-9"}`),
		Frame:     "#nosuchframe",
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatalf("invoke in a missing frame: %v", err)
	}
	if missing.OK || missing.Status != snapshot.PageToolNotFound || !strings.Contains(missing.Error, "webmcp frame not found") {
		t.Fatalf("missing frame invoke = %+v, want a named frame error", missing)
	}
}

// An invocation whose document goes away must be reported as lost the moment it
// is noticed. Waiting out the caller's timeout on a page that stopped existing
// is the failure this registry's document nonce exists to prevent.
func TestPageToolInvocationIsLostWhenThePageNavigatesAway(t *testing.T) {
	srv := servePage(t, slowToolPage)
	elsewhere := servePage(t, `<!doctype html><html><body><h1>somewhere else</h1></body></html>`)
	ctx := armedWebMCPTab(t, srv.URL)
	eval := chromedpEvaluator(ctx)

	started, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
		Name:      "export_ledger",
		Arguments: []byte(`{"format":"csv"}`),
		Detach:    true,
		Validate:  true,
	})
	if err != nil {
		t.Fatalf("detached invoke: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(elsewhere.URL),
		chromedp.WaitVisible("h1", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("navigate away: %v", err)
	}

	// A generous timeout: the point is that it returns well inside it.
	deadline := 20 * time.Second
	begun := time.Now()
	lost, err := snapshot.AwaitPageTool(ctx, eval, started.ID, deadline)
	if err != nil {
		t.Fatalf("poll after navigation: %v", err)
	}
	if lost.Status != snapshot.PageToolLost || lost.OK || lost.TimedOut {
		t.Fatalf("poll after navigation = %+v, want status lost", lost)
	}
	// The poll walks only the windows of the tab it landed in, so the message has
	// to say what was observed — no document HERE holds the id — and name the
	// thing an agent can act on, rather than asserting a navigation it did not
	// establish. A page tool that opens a tab moves the active one, and that is
	// the same report on a document that is alive and still running.
	for _, want := range []string{"no document in the polled tab holds", "navigated away", "tab_id"} {
		if !strings.Contains(lost.Error, want) {
			t.Fatalf("lost invocation error = %q, want it to name %q", lost.Error, want)
		}
	}
	if elapsed := time.Since(begun); elapsed > deadline/2 {
		t.Fatalf("lost invocation took %s to report, which is waiting out the timeout", elapsed)
	}

	// An id this runtime never minted is a different mistake, and says so.
	if _, err := snapshot.AwaitPageTool(ctx, eval, "not-an-id", 0); !errors.Is(err, snapshot.ErrPageToolIDUnrecognised) {
		t.Fatalf("bogus id error = %v, want ErrPageToolIDUnrecognised", err)
	}
}

// A page tool that throws the instant it is called has already failed by the
// time the start script returns. Detached, that report is the whole answer the
// agent gets, so reporting ok:true and dropping the message would send it off to
// collect work that never ran.
func TestSynchronouslyFailingPageToolIsReportedAtTheStart(t *testing.T) {
	srv := servePage(t, slowToolPage)
	ctx := armedWebMCPTab(t, srv.URL)
	eval := chromedpEvaluator(ctx)

	for _, tc := range []struct {
		name   string
		detach bool
	}{
		{name: "detached", detach: true},
		{name: "waited", detach: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{
				Name:     "throw_now",
				Detach:   tc.detach,
				Validate: true,
				Timeout:  5 * time.Second,
			})
			if err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if got.OK || got.Status != snapshot.PageToolFailed {
				t.Fatalf("invoke = %+v, want ok:false with status failed", got)
			}
			if !strings.Contains(got.Error, "ledger is locked") {
				t.Fatalf("invoke error = %q, want the page tool's own message", got.Error)
			}
			if got.Detached || strings.Contains(got.Note, "collect it") {
				t.Fatalf("invoke = %+v, want no instruction to collect an invocation that already failed", got)
			}
		})
	}
}

// scriptedEvaluator answers page-tool expressions without a browser, so the
// parts of the invocation path that are pure daemon logic — which id the
// expression carries, what a wait returns when it is cut short — stay testable
// on a machine with no Chrome.
type scriptedEvaluator struct {
	mu          sync.Mutex
	expressions []string
	respond     func(expression string, call int) (any, error)
}

func (s *scriptedEvaluator) eval(_ context.Context, expression string) (any, error) {
	s.mu.Lock()
	s.expressions = append(s.expressions, expression)
	call := len(s.expressions)
	s.mu.Unlock()
	return s.respond(expression, call)
}

func (s *scriptedEvaluator) first() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.expressions) == 0 {
		return ""
	}
	return s.expressions[0]
}

// Validation trimmed a copy of the id and the expression then embedded the
// original, so a padded id passed validation, was looked up verbatim, found
// nothing, and came back as "lost" — the misleading answer rather than a
// rejected id. The value that reaches the page has to be the validated one.
func TestPageToolIDIsTrimmedBeforeItReachesThePage(t *testing.T) {
	const id = "0a1b2c3d4e5f6071-3"
	running := map[string]any{"ok": false, "id": id, "status": "running"}

	for _, tc := range []struct {
		name  string
		given string
	}{
		{name: "surrounding spaces", given: "  " + id + "  "},
		{name: "trailing newline", given: id + "\n"},
		{name: "already clean", given: id},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("result", func(t *testing.T) {
				rec := &scriptedEvaluator{respond: func(string, int) (any, error) { return running, nil }}
				if _, err := snapshot.AwaitPageTool(context.Background(), rec.eval, tc.given, 0); err != nil {
					t.Fatalf("await: %v", err)
				}
				if got, want := rec.first(), snapshot.BuildPageToolResultExpression(id); got != want {
					t.Fatalf("poll expression carries the untrimmed id:\n%s\nwant\n%s", got, want)
				}
			})
			t.Run("cancel", func(t *testing.T) {
				rec := &scriptedEvaluator{respond: func(string, int) (any, error) { return running, nil }}
				if _, err := snapshot.CancelPageTool(context.Background(), rec.eval, tc.given); err != nil {
					t.Fatalf("cancel: %v", err)
				}
				if got, want := rec.first(), snapshot.BuildPageToolCancelExpression(id); got != want {
					t.Fatalf("cancel expression carries the untrimmed id:\n%s\nwant\n%s", got, want)
				}
			})
		})
	}
}

// A wait can end without the invocation ending: the request is cancelled, the
// tab closes, an evaluate fails outright. The invocation is untouched in the
// page, so the id has to come back in the report — an id that survives only
// inside an error string is not something an agent can poll or cancel with.
func TestPageToolWaitKeepsTheIDWhenItIsCutShort(t *testing.T) {
	const id = "0a1b2c3d4e5f6071-4"
	running := map[string]any{"ok": false, "id": id, "status": "running"}

	t.Run("cancelled context", func(t *testing.T) {
		rec := &scriptedEvaluator{respond: func(string, int) (any, error) { return running, nil }}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(120 * time.Millisecond)
			cancel()
		}()
		got, err := snapshot.AwaitPageTool(ctx, rec.eval, id, 30*time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("await error = %v, want context.Canceled", err)
		}
		if got.ID != id || !got.Interrupted || got.TimedOut {
			t.Fatalf("cancelled wait = %+v, want the id back marked interrupted", got)
		}
	})

	t.Run("evaluate fails outright", func(t *testing.T) {
		boom := errors.New("tab was closed")
		rec := &scriptedEvaluator{respond: func(string, int) (any, error) { return nil, boom }}
		got, err := snapshot.AwaitPageTool(context.Background(), rec.eval, id, time.Second)
		if !errors.Is(err, boom) {
			t.Fatalf("await error = %v, want the evaluate failure", err)
		}
		if got.ID != id || !got.Interrupted {
			t.Fatalf("failed wait = %+v, want the id back marked interrupted", got)
		}
	})

	t.Run("a started invocation survives a failed collection", func(t *testing.T) {
		boom := errors.New("tab was closed")
		rec := &scriptedEvaluator{respond: func(_ string, call int) (any, error) {
			if call == 1 {
				return map[string]any{"ok": true, "id": id, "name": "export_ledger", "status": "running"}, nil
			}
			return nil, boom
		}}
		got, err := snapshot.InvokePageTool(context.Background(), rec.eval, snapshot.PageToolInvokeOptions{
			Name:      "export_ledger",
			Arguments: []byte(`{"format":"csv"}`),
			Timeout:   time.Second,
		})
		if !errors.Is(err, boom) {
			t.Fatalf("invoke error = %v, want the collection failure", err)
		}
		if got.ID != id || !got.Interrupted {
			t.Fatalf("failed collection = %+v, want the started invocation still addressable", got)
		}
	})
}
