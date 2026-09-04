package snapshot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// newHeadlessSettleCtx spins up a headless Chrome with the same timer-throttling
// flags production uses (see internal/cdp/launcher.go). Disabling background timer
// throttling is load-bearing for the settle quiesce path: without it, headless
// Chrome clamps setTimeout to ~1Hz, which would inflate the ~40ms quiet window to
// ~1s and make the fast-path assertion flaky. Skips when no browser is available.
func newHeadlessSettleCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.NoSandbox,
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		chromedp.WSURLReadTimeout(45*time.Second),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	ctx, ctxCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(ctx); err != nil {
		ctxCancel()
		allocCancel()
		t.Skipf("headless chrome unavailable: %v", err)
	}
	cancel := func() {
		ctxCancel()
		allocCancel()
	}
	return ctx, cancel
}

// quiesceFixture mutates the DOM in a short early burst, then goes quiet — the
// canonical "action caused a small reaction then the page settled" case. Settle
// should resolve via the mutation-quiesce path well before the cap.
//
// The burst (a handful of mutations spread across the first ~70ms) rather than a
// single +5ms mutation makes the test robust under parallel load: Settle is
// invoked AFTER navigation returns, so a single very-early mutation could land and
// quiesce before Settle attaches its observer, leaving no signal to catch and
// forcing a (spurious) cap timeout. With a brief burst, at least one mutation
// reliably lands after the observer is attached, then the page goes quiet and the
// quiesce path fires well under the 150ms cap.
const quiesceFixture = `data:text/html,` +
	`<html><body><div id="x">start</div>` +
	`<script>` +
	`var n=0;var iv=setInterval(function(){var e=document.getElementById('x');e.textContent='mutated '+(++n);if(n>=6){clearInterval(iv);}},8);` +
	`</script>` +
	`</body></html>`

// staticFixture never mutates, navigates, or issues a network request after load.
// There is no signal that the page has "reacted", so Settle cannot resolve early
// and must degrade to the cap — proving the hard cap bounds the worst case.
const staticFixture = `data:text/html,` +
	`<html><body><div>static</div></body></html>`

// TestSettleResolvesFastOnQuiesce proves the fast path: a page that reacts then
// goes quiet settles well under the old fixed 150ms post-action delay, returning
// via the mutation-quiesce path. This is the latency win — the action no longer
// pays the full fixed sleep when the page has demonstrably settled.
func TestSettleResolvesFastOnQuiesce(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()

	if err := chromedp.Run(runCtx, chromedp.Navigate(quiesceFixture)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	const capMS = 150
	// This measures wall-clock time, so a machine busy running the rest of the
	// suite in parallel can starve the in-page quiesce detector out of its
	// window and settle at the cap — a fact about the load, not about brw. A
	// real regression caps on every attempt, so requiring one fast settle out of
	// a few keeps the assertion's teeth without the false failures.
	const attempts = 3
	var res SettleResult
	var wall time.Duration
	var observed []string

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// Re-navigate so the fixture's DOM mutation and quiesce window run
			// again; a second Settle on an already-quiet page has no early
			// signal left to find.
			if err := chromedp.Run(runCtx, chromedp.Navigate(quiesceFixture)); err != nil {
				t.Fatalf("re-navigate: %v", err)
			}
		}
		wallStart := time.Now()
		var err error
		res, err = Settle(runCtx, capMS)
		wall = time.Since(wallStart)
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
		observed = append(observed, fmt.Sprintf("settledMs=%d reason=%q wall=%v", res.SettledMS, res.Reason, wall))

		// (b) Fast path: must return well under the old fixed 150ms delay. The
		// DOM mutation lands at ~5ms and the quiesce window is ~40ms, so the
		// in-page settle should report well under 100ms, resolved by an early
		// signal (quiesce, network, or navigation) rather than the cap.
		if res.SettledMS < 100 && res.Reason != "cap" && wall < capMS*time.Millisecond {
			break
		}
		if attempt == attempts-1 {
			t.Fatalf("no fast settle in %d attempts, so the adaptive settle is not beating the fixed %dms sleep: %v",
				attempts, capMS, observed)
		}
	}

	if res.Cap != capMS {
		t.Fatalf("expected reported cap %d, got %d", capMS, res.Cap)
	}
}

// TestSettleCapsWorstCase proves the safety contract: a page with no settle signal
// resolves at the cap (reason "cap") and never overshoots it — so the worst case is
// exactly today's fixed delay, never slower.
func TestSettleCapsWorstCase(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()

	if err := chromedp.Run(runCtx, chromedp.Navigate(staticFixture)); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	// Let load fully complete so the navigation's own resource entry does not race
	// the settle and resolve it as "network".
	if err := chromedp.Run(runCtx, chromedp.WaitReady("body")); err != nil {
		t.Fatalf("wait ready: %v", err)
	}

	const capMS = 120
	res, err := Settle(runCtx, capMS)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if res.Reason != "cap" {
		t.Fatalf("expected cap timeout on a static page, got reason=%q settledMs=%d", res.Reason, res.SettledMS)
	}
	// Hard cap: must not meaningfully overshoot the cap. Allow a small scheduling
	// slack for the in-page performance.now() read on the cap timer.
	if res.SettledMS > capMS+50 {
		t.Fatalf("settle overshot the %dms cap: settledMs=%d", capMS, res.SettledMS)
	}
}

// TestSettleZeroCapReturnsImmediately proves a zero cap is a no-op fast return, so
// call sites that pass no delay never block.
func TestSettleZeroCapReturnsImmediately(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()

	if err := chromedp.Run(runCtx, chromedp.Navigate(staticFixture)); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	res, err := Settle(runCtx, 0)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if res.Reason != "cap" || res.SettledMS > 25 {
		t.Fatalf("expected immediate cap return, got reason=%q settledMs=%d", res.Reason, res.SettledMS)
	}
}

// TestPreArmedSettleCatchesSynchronousMutation is the regression test for the
// latency bug that a post-action observer cannot see: the click handler mutates
// synchronously before a following CDP command can attach anything.
func TestPreArmedSettleCatchesSynchronousMutation(t *testing.T) {
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	fixture := `data:text/html,<button id="b">go</button><output id="o">0</output><script>b.onclick=()=>o.textContent='1'</script>`
	if err := chromedp.Run(runCtx, chromedp.Navigate(fixture), chromedp.WaitReady("#b")); err != nil {
		t.Fatal(err)
	}
	handle, err := ArmSettle(runCtx, 150)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`document.getElementById('b').click()`, nil)); err != nil {
		t.Fatal(err)
	}
	result, err := AwaitSettle(runCtx, handle)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != "quiesce" || result.SettledMS >= 100 || time.Since(started) >= 130*time.Millisecond {
		t.Fatalf("pre-armed settle missed synchronous mutation: result=%+v wall=%s", result, time.Since(started))
	}
}

func TestPreArmedSettleWaitsForRenderAfterNetworkResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/signal" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><button id="b">load</button><output id="o">pending</output><script>
			b.onclick=()=>fetch('/signal').then(()=>setTimeout(()=>{o.textContent='rendered'},20))
		</script>`))
	}))
	defer server.Close()
	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	if err := chromedp.Run(runCtx, chromedp.Navigate(server.URL), chromedp.WaitReady("#b")); err != nil {
		t.Fatal(err)
	}
	handle, err := ArmSettle(runCtx, 150)
	if err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`document.getElementById('b').click()`, nil)); err != nil {
		t.Fatal(err)
	}
	result, err := AwaitSettle(runCtx, handle)
	if err != nil {
		t.Fatal(err)
	}
	var rendered string
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`document.getElementById('o').textContent`, &rendered)); err != nil {
		t.Fatal(err)
	}
	if rendered != "rendered" || result.SettledMS < 35 {
		t.Fatalf("settled before post-response render: result=%+v output=%q", result, rendered)
	}
}
